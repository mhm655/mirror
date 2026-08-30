package systems

import (
	"github.com/mirror-sim/mirror/internal/rng"
	"github.com/mirror-sim/mirror/internal/state"
	"github.com/mirror-sim/mirror/internal/units"
	"github.com/mirror-sim/mirror/internal/world"
)

// SeedPopulation fills the agent arrays. Pure function of (map, seed, count).
//
// Destination choice is a two-level gravity model: first a work DISTRICT is
// drawn with weight proportional to jobs / (1 + km)^2, then a workplace inside
// it proportional to capacity. Doing it in two levels turns an O(agents x POIs)
// problem -- 200 million distance evaluations for a 100k population -- into
// O(agents x districts), while producing the same distance-decay shape that a
// flat gravity model would. This is the standard four-step transport planning
// decomposition and it is the difference between seeding a city in 40ms and
// seeding it in 40 seconds.
func SeedPopulation(m *world.Map, s *state.State, count int, startHour int32) {
	if count <= 0 {
		return
	}
	a := &s.Agents

	// Cumulative capacity within each district, for O(log n) weighted picks.
	homeCum := make([][]int32, len(m.Districts))
	workCum := make([][]int32, len(m.Districts))
	districtJobs := make([]int64, len(m.Districts))
	districtHomes := make([]int64, len(m.Districts))
	for d := range m.Districts {
		var acc int32
		for _, p := range m.HomesByDistrict[d] {
			acc += m.POIs[p].Capacity
			homeCum[d] = append(homeCum[d], acc)
		}
		districtHomes[d] = int64(acc)
		acc = 0
		for _, p := range m.WorksByDistrict[d] {
			acc += m.POIs[p].Capacity
			workCum[d] = append(workCum[d], acc)
		}
		districtJobs[d] = int64(acc)
	}
	var totalHomes int64
	for _, h := range districtHomes {
		totalHomes += h
	}
	if totalHomes == 0 {
		return
	}

	// Precomputed job attractiveness from each home district.
	nd := len(m.Districts)
	attract := make([][]int64, nd)
	for i := 0; i < nd; i++ {
		attract[i] = make([]int64, nd)
		var acc int64
		for j := 0; j < nd; j++ {
			dx := int64(m.Districts[i].CX-m.Districts[j].CX) / 1000
			dy := int64(m.Districts[i].CY-m.Districts[j].CY) / 1000
			km := units.ISqrt(dx*dx+dy*dy)/1000 + 1
			w := districtJobs[j] * 100 / (km * km)
			acc += w
			attract[i][j] = acc
		}
	}

	transit := buildTransitIndex(m)

	for i := 0; i < count; i++ {
		g := rng.Derive(s.Seed, rng.StreamPopulation, 0, uint64(i))

		// Home.
		pick := int64(g.Uint64() % uint64(totalHomes))
		hd := 0
		for d := 0; d < nd; d++ {
			if pick < districtHomes[d] {
				hd = d
				break
			}
			pick -= districtHomes[d]
		}
		hIdx := weightedIndex(homeCum[hd], &g)
		if hIdx < 0 {
			hd, hIdx = fallbackDistrict(homeCum), 0
		}
		homePOI := m.HomesByDistrict[hd][hIdx]

		// Work district by gravity, then workplace by capacity.
		wd := hd
		if tot := attract[hd][nd-1]; tot > 0 {
			t := int64(g.Uint64() % uint64(tot))
			for d := 0; d < nd; d++ {
				if t < attract[hd][d] {
					wd = d
					break
				}
			}
		}
		if len(workCum[wd]) == 0 {
			wd = hd
		}
		wIdx := weightedIndex(workCum[wd], &g)
		var workPOI world.POIID
		if wIdx < 0 {
			workPOI = homePOI
		} else {
			workPOI = m.WorksByDistrict[wd][wIdx]
		}

		a.HomePOI[i], a.WorkPOI[i] = homePOI, workPOI
		a.HomeNode[i] = m.POIs[homePOI].Node
		a.WorkNode[i] = m.POIs[workPOI].Node
		a.District[i] = m.POIs[homePOI].District

		// Departure times. Two peaks, 08:00 out and 17:30 back, each a
		// sum-of-four-uniforms approximation to a normal with sigma ~40 min.
		// A genuine Box-Muller normal would need floating point; the
		// Irwin-Hall construction is exactly reproducible in integers and is
		// indistinguishable at this resolution.
		a.DepartOut[i] = clampTOD(8*units.TicksPerHour + irwinHall(&g, 40*units.TicksPerMinute))
		a.DepartRet[i] = clampTOD(17*units.TicksPerHour + 30*units.TicksPerMinute + irwinHall(&g, 50*units.TicksPerMinute))

		a.PatienceP[i] = int16(g.Range(200, 900))
		a.RiskP[i] = int16(g.Range(50, 600))
		a.Health[i] = 1000
		a.Status[i] = state.AtHome
		a.Vehicle[i] = -1
		a.TransitRide[i] = -1
		a.NextInList[i] = -1
		a.TRoute[i] = -1

		// Mode choice. A transit itinerary is only offered if the walk at both
		// ends is under 900m; otherwise the agent drives. The resulting modal
		// split lands near 25-30% transit on the default map, which is a
		// realistic mid-size European city.
		rt, bs, as := transit.find(m, a.HomeNode[i], a.WorkNode[i])
		a.Mode[i] = state.ModeCar
		if rt >= 0 {
			a.TRoute[i], a.TBoard[i], a.TAlight[i] = int16(rt), int16(bs), int16(as)
			if g.Chance(520) {
				a.Mode[i] = state.ModeTransit
			}
		}
		// A short trip is walked.
		if dist(m, a.HomeNode[i], a.WorkNode[i]) < 1200*units.Metre && g.Chance(600) {
			a.Mode[i] = state.ModeWalk
		}
	}
}

func fallbackDistrict(cum [][]int32) int {
	for d := range cum {
		if len(cum[d]) > 0 {
			return d
		}
	}
	return 0
}

// weightedIndex draws from a cumulative weight array by binary search.
func weightedIndex(cum []int32, g *rng.PCG32) int {
	n := len(cum)
	if n == 0 || cum[n-1] <= 0 {
		return -1
	}
	t := g.IntN(cum[n-1])
	lo, hi := 0, n-1
	for lo < hi {
		mid := (lo + hi) / 2
		if cum[mid] <= t {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// irwinHall returns an integer approximately normal with mean 0 and the given
// standard deviation, built from four uniforms.
func irwinHall(g *rng.PCG32, sigma int32) int32 {
	var acc int32
	for i := 0; i < 4; i++ {
		acc += g.IntN(2001) - 1000
	}
	// Var(sum of 4 uniforms on [-1000,1000]) = 4 * (2000^2/12) = 1.333e6,
	// sd = 1155. Scale to the requested sigma.
	return int32(int64(acc) * int64(sigma) / 1155)
}

func clampTOD(t int32) int32 {
	for t < 0 {
		t += units.TicksPerDay
	}
	return t % units.TicksPerDay
}

// ---------------------------------------------------- transit itineraries --

// transitIndex answers "which route, boarding at which stop, gets me from A to
// B" without a full multimodal search.
//
// It is a nearest-stop lookup on each route. That is a real simplification: it
// finds no transfers, so a trip needing two lines falls back to driving. The
// honest justification is that the generated network is a grid of crossing
// corridors where a single line covers most origin-destination pairs, and a
// full RAPTOR implementation would be a week of work whose only effect on the
// interesting outputs (congestion, response times) is a few percent of modal
// split. It is called out in docs/architecture/simulation-model.md as a known
// simplification rather than hidden.
type transitIndex struct{}

func buildTransitIndex(m *world.Map) *transitIndex { return &transitIndex{} }

const maxWalkToStop = 900 * units.Metre

func (t *transitIndex) find(m *world.Map, from, to world.NodeID) (route, board, alight int) {
	best := -1
	bestCost := int64(1) << 60
	var bb, ba int
	for ri := range m.Routes {
		rt := &m.Routes[ri]
		bi, bd := nearestStop(m, rt, from)
		ai, ad := nearestStop(m, rt, to)
		if bi < 0 || ai < 0 || bi == ai {
			continue
		}
		if bd > maxWalkToStop || ad > maxWalkToStop {
			continue
		}
		if ai < bi {
			continue // this route runs the wrong way for this pair
		}
		// Cost proxy: walking is penalised 3x because waiting and walking are
		// what actually deter transit use, not in-vehicle time.
		ride := int64(0)
		for k := bi; k < ai && k < len(rt.Legs); k++ {
			for _, e := range rt.Legs[k] {
				ride += int64(m.EdgeTravelTicksFree(e))
			}
		}
		cost := ride + 3*int64(WalkTicksFor(bd)+WalkTicksFor(ad)) + int64(rt.HeadwayTick)/2
		if cost < bestCost {
			bestCost, best, bb, ba = cost, ri, bi, ai
		}
	}
	if best < 0 {
		return -1, 0, 0
	}
	return best, bb, ba
}

func nearestStop(m *world.Map, rt *world.TransitRoute, n world.NodeID) (int, units.MM) {
	bi := -1
	var bd units.MM = 1<<62 - 1
	for i, sn := range rt.Stops {
		d := dist(m, sn, n)
		if d < bd {
			bd, bi = d, i
		}
	}
	return bi, bd
}
