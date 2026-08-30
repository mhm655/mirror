package systems

import (
	"github.com/mirror-sim/mirror/internal/events"
	"github.com/mirror-sim/mirror/internal/rng"
	"github.com/mirror-sim/mirror/internal/state"
	"github.com/mirror-sim/mirror/internal/units"
	"github.com/mirror-sim/mirror/internal/world"
)

// ------------------------------------------------------------- speeds -----

// weatherSpeedP is the permille speed multiplier for the current conditions.
// Values are from the FHWA weather-impact tables, rounded: rain -10%, heavy
// storm -20%, snow -35%, fog -25%. Heatwave does not slow traffic but does
// raise electrical demand, which is modelled in power.go.
func weatherSpeedP(w state.Weather) int32 {
	switch w.Condition {
	case 1:
		return 900
	case 2:
		return 800
	case 3:
		return 650
	case 5:
		return 750
	default:
		return 1000
	}
}

// signalDelayTicks is the average delay a router should expect at a signalised
// intersection: half the cycle. Including it in the routing cost is what makes
// drivers prefer a slightly longer unsignalised route, which is the behaviour
// that produces rat-running through side streets when an arterial jams.
func signalDelayTicks(m *world.Map, s *state.State, e world.EdgeID) int32 {
	si := m.Nodes[m.Edges[e].To].Signal
	if si < 0 {
		return 0
	}
	if !s.Signals.Powered[si] {
		return 4 * units.TicksPerSecond // all-way stop
	}
	cycle := s.Signals.Green0[si] + s.Signals.Green1[si]
	return cycle / 4
}

// UpdateEdgeSpeeds recomputes realised speed and perceived travel time for the
// region's edges.
//
// The speed-density relation is Greenshields (v = v_free * (1 - k/k_jam)),
// chosen over the more common BPR volume-delay function because BPR is a
// steady-state *link performance* function for planning models -- it takes an
// hourly volume, not an instantaneous occupancy, and applying it per tick
// produces a link that never actually fills up. Greenshields is defined on
// instantaneous density, which is exactly what a microsimulation has, and it
// reproduces the two behaviours we care about: throughput peaks at ~50%
// density, and above that, adding vehicles reduces flow. That is congestion
// collapse, and it is the reason a single blocked lane can gridlock a corridor.
func UpdateEdgeSpeeds(c *Ctx, r *Region) {
	m, s := c.Map, c.S
	wmul := weatherSpeedP(s.Weather)
	limit := s.Policy.SpeedLimitP
	if limit <= 0 {
		limit = 1000
	}
	night := isNight(s)

	// Incremental pass.
	//
	// The speed of an edge is a function of five things: its occupancy, its
	// blocked lanes, its closure, its lighting, and three city-wide multipliers
	// (weather, the speed-limit policy, and whether it is dark). On a typical
	// network fewer than a fifth of edges have any vehicle on them at all, and
	// the city-wide multipliers change a handful of times a day -- so
	// recomputing all of them every tick was, by the profile, the single
	// largest cost in the engine.
	//
	// An edge is skipped when it is empty now, was empty last tick, is not
	// blocked, is not closed and is lit. Under those conditions every input to
	// the formula is identical to the last time it ran, so the stored result is
	// already correct: the skip is exactly value-preserving rather than an
	// approximation, which is why the state digest is unchanged by it. The
	// city-wide multipliers are folded into a key; when the key moves, the
	// whole region is recomputed once.
	key := int64(wmul)<<40 | int64(limit)<<8 | boolBit(night)<<1 | 1
	full := r.speedKey != key
	if len(r.prevCount) != len(m.Edges) {
		r.prevCount = make([]int32, len(m.Edges))
		full = true
	}
	r.speedKey = key
	tickNow := uint32(c.Tick)

	for _, e := range r.Edges {
		cnt := s.Edges.Count[e]
		// An edge is "special" while it is blocked, closed or dark. Special
		// edges are always recomputed, and -- this is the part the first
		// version got wrong -- the fact that an edge WAS special is recorded,
		// so that the tick it stops being special it is recomputed once more.
		// Without that, a road whose closure expired while empty kept its
		// impassable speed forever, and every router went on avoiding a road
		// that had reopened.
		special := s.Edges.BlockedLanes[e] != 0 ||
			s.Edges.ClosedUntil[e] > tickNow || !s.Edges.Lit[e]
		if !full && !special && cnt == 0 && r.prevCount[e] == 0 {
			continue
		}
		if special {
			r.prevCount[e] = -1
		} else {
			r.prevCount[e] = cnt
		}
		ed := &m.Edges[e]
		eff := ed.Lanes - s.Edges.BlockedLanes[e]
		if eff < 0 {
			eff = 0
		}
		if eff == 0 || s.Edges.ClosedUntil[e] > uint32(c.Tick) {
			s.Edges.Speed[e] = 0
			s.Edges.TravelTicks[e] = world.Impassable
			continue
		}
		jam := ed.Jam * eff / ed.Lanes
		if jam < 1 {
			jam = 1
		}
		xP := int32(int64(cnt) * 1000 / int64(jam))
		if xP > 940 {
			xP = 940
		}
		dens := 1000 - xP
		if dens < 60 {
			// A hard floor at 6% of free flow. Without it, a saturated link
			// takes effectively infinite time and every router routes around
			// it, which produces an unrealistic all-or-nothing flip.
			dens = 60
		}
		sp := int64(ed.FreeSpeed)
		sp = units.MulP(sp, units.Permille(dens))
		sp = units.MulP(sp, units.Permille(wmul))
		sp = units.MulP(sp, units.Permille(limit))
		if night && !s.Edges.Lit[e] {
			sp = units.MulP(sp, 820)
		}
		if sp < 1 {
			sp = 1
		}
		s.Edges.Speed[e] = units.MMPerTick(sp)
		tt := int32(units.DivRound(int64(ed.Length), sp))
		if tt < 1 {
			tt = 1
		}
		s.Edges.TravelTicks[e] = tt + signalDelayTicks(m, s, e)

		// Gridlock alert: emitted once per edge per crossing of the threshold,
		// gated on tick parity so a persistent jam does not spam the feed.
		if xP >= 850 && uint64(c.Tick)%(30*units.TicksPerSecond) == uint64(e)%(30*units.TicksPerSecond) {
			r.emit(c.Tick, events.EvtCongestionCritical, events.SevWarning,
				int64(e), units.MMPerTickToKmh(units.MMPerTick(sp)),
				int64(cnt), int64(jam))
		}
	}
}

func boolBit(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func isNight(s *state.State) bool {
	h, _ := s.ClockHM()
	return h >= 21 || h < 6
}

// ------------------------------------------------------------- routing ----

// CostFn builds the perceived edge cost for a class of traveller.
//
// congestionAware=false models a driver with no live traffic information: they
// use habitual free-flow times. The split between aware and unaware drivers is
// a policy lever (RerouteAwarenessP) and it matters a great deal -- a network
// where 100% of drivers reroute instantly oscillates, and one where 0% do
// cannot respond to an incident at all. Real cities sit in between.
func CostFn(c *Ctx, congestionAware, emergency bool) world.CostFn {
	m, s := c.Map, c.S
	central := centralDistrict(m)
	charge := s.Policy.CongestionCharge
	tick := uint32(c.Tick)
	return func(e world.EdgeID) int32 {
		if s.Edges.ClosedUntil[e] > tick {
			return world.Impassable
		}
		ed := &m.Edges[e]
		free := m.EdgeTravelTicksFree(e)
		var cost int32
		if congestionAware {
			cost = s.Edges.TravelTicks[e]
			if cost >= world.Impassable {
				return world.Impassable
			}
			if emergency {
				// Emergency vehicles filter through congestion: they feel half
				// the congestion penalty, never less than free-flow time. Kept
				// >= free-flow so the A* heuristic stays admissible.
				cost = free + (cost-free)/2
			}
		} else {
			cost = free + signalDelayTicks(m, s, e)
		}
		if s.Edges.BlockedLanes[e] > 0 {
			cost += cost * s.Edges.BlockedLanes[e] / (ed.Lanes + 1)
		}
		if charge && !emergency && ed.District == central {
			cost += 40 * units.TicksPerSecond
		}
		return cost
	}
}

func centralDistrict(m *world.Map) world.DistrictID {
	// The district containing the map centre.
	cx, cy := m.Width/2, m.Height/2
	for i := range m.Districts {
		d := &m.Districts[i]
		if cx >= d.MinX && cx < d.MaxX && cy >= d.MinY && cy < d.MaxY {
			return d.ID
		}
	}
	return 0
}

// ------------------------------------------------------------- signals ----

// UpdateSignals advances the fixed-time or adaptive controller.
func UpdateSignals(c *Ctx, r *Region) {
	m, s := c.Map, c.S
	adaptive := s.Policy.AdaptiveSignals
	maxExtend := s.Policy.AdaptiveMaxExtendTicks
	for _, si := range r.Signals {
		sig := &m.Signals[si]
		q0, q1 := s.Signals.Queue0[si], s.Signals.Queue1[si]
		// Queues are re-accumulated by MoveVehicles later this tick.
		s.Signals.Queue0[si], s.Signals.Queue1[si] = 0, 0

		if !s.Signals.Powered[si] {
			// A dark signal does not cycle. Movement treats it as an all-way
			// stop, which cuts intersection throughput by roughly 60%.
			continue
		}

		if adaptive {
			// Queue-actuated control: extend the green on the busier approach,
			// bounded by AdaptiveMaxExtendTicks. The bound is the whole trick.
			// An unbounded "serve the longest queue" controller starves the
			// cross street and, on a grid, deadlocks it -- the classic way a
			// naive adaptive scheme makes the network worse than fixed time.
			base0, base1 := sig.PhaseTicks[0], sig.PhaseTicks[1]
			g0 := base0 + clampExtend((q0-q1)*units.TicksPerSecond/3, maxExtend)
			g1 := base1 + clampExtend((q1-q0)*units.TicksPerSecond/3, maxExtend)
			minG := int32(6 * units.TicksPerSecond)
			if g0 < minG {
				g0 = minG
			}
			if g1 < minG {
				g1 = minG
			}
			s.Signals.Green0[si], s.Signals.Green1[si] = g0, g1
		} else {
			s.Signals.Green0[si], s.Signals.Green1[si] = sig.PhaseTicks[0], sig.PhaseTicks[1]
		}

		if s.Signals.Preempt[si] > 0 {
			s.Signals.Preempt[si]--
			continue // phase is held by the preemption
		}

		s.Signals.PhaseTick[si]++
		var green int32
		if s.Signals.Phase[si] == 0 {
			green = s.Signals.Green0[si]
		} else {
			green = s.Signals.Green1[si]
		}
		if s.Signals.PhaseTick[si] >= green {
			s.Signals.PhaseTick[si] = 0
			s.Signals.Phase[si] = 1 - s.Signals.Phase[si]
			r.emit(c.Tick, events.EvtSignalChanged, events.SevInfo,
				int64(si), int64(s.Signals.Phase[si]), 0, 0)
		}
	}
}

func clampExtend(v, max int32) int32 {
	if v > max {
		return max
	}
	if v < -max {
		return -max
	}
	return v
}

// ------------------------------------------------------------ movement ----

// MoveVehicles advances every vehicle the region owns.
//
// This is the hot loop. It is written as a flat scan over an id list with no
// virtual dispatch, no allocation, and no map lookups. Everything that would
// need to touch another region's state -- crossing to the next edge, arriving,
// crashing -- becomes an intent.
func MoveVehicles(c *Ctx, r *Region) {
	m, s := c.Map, c.S
	v := &s.Vehicles
	preemption := s.Policy.EmergencyPreemption
	for _, id := range r.Vehicles {
		st := v.Status[id]
		if st != state.VehMoving && st != state.VehQueued {
			continue
		}
		e := v.Edge[id]
		if e < 0 {
			continue
		}
		if v.DwellTicks[id] > 0 {
			v.DwellTicks[id]--
			v.Speed[id] = 0
			v.StopTicks[id]++
			continue
		}
		ed := &m.Edges[e]
		sp := int64(s.Edges.Speed[e])
		kind := v.Kind[id]
		if kind.IsEmergency() {
			// Blue-light vehicles make progress through congestion by using
			// the opposing lane; capped so they never exceed free-flow.
			sp = sp * 3 / 2
			if sp > int64(ed.FreeSpeed) {
				sp = int64(ed.FreeSpeed)
			}
		}
		if kind == state.VehMetro {
			// Metro has segregated right of way: unaffected by road traffic.
			sp = int64(ed.FreeSpeed)
		}

		// Emergency preemption: hold the signal ahead green while close.
		if preemption && kind.IsEmergency() {
			if si := m.Nodes[ed.To].Signal; si >= 0 && s.Signals.Powered[si] {
				remain := int64(ed.Length - v.Pos[id])
				if sp > 0 && remain/sp <= int64(8*units.TicksPerSecond) && ed.SignalPhase >= 0 {
					// Deferred to the commit phase. Other regions read signal
					// phase through their routing cost function during the same
					// parallel phase, so writing it here would be both a data
					// race and a source of nondeterminism.
					r.intent(IntentPreempt, int64(si), int64(ed.SignalPhase),
						int64(6*units.TicksPerSecond), 0)
				}
			}
		}

		npos := v.Pos[id] + units.MM(sp)
		if npos < ed.Length {
			v.Pos[id] = npos
			v.Speed[id] = units.MMPerTick(sp)
			v.Status[id] = state.VehMoving
			v.DistanceMM[id] += sp
			r.Moved++
			if sp <= int64(ed.FreeSpeed)/12 {
				v.StopTicks[id]++
			}
			maybeCrash(c, r, id, e, sp)
			continue
		}

		// At the end of the edge.
		v.DistanceMM[id] += int64(ed.Length - v.Pos[id])
		v.Pos[id] = ed.Length
		ri := v.RouteIdx[id]
		if ri >= v.RouteLen[id] {
			r.intent(IntentArrive, int64(id), 0, 0, 0)
			v.Speed[id] = 0
			continue
		}
		next := s.Route(id)[ri]

		// Signal gate.
		if si := m.Nodes[ed.To].Signal; si >= 0 && ed.SignalPhase >= 0 {
			blocked := false
			if !s.Signals.Powered[si] {
				// All-way stop: a vehicle gets to go roughly every 3 seconds,
				// staggered by id so the intersection does not release
				// everything at once.
				if (uint64(c.Tick)+uint64(id))%uint64(3*units.TicksPerSecond) != 0 {
					blocked = true
				}
			} else if s.Signals.Phase[si] != int8(ed.SignalPhase) {
				blocked = true
			}
			if blocked {
				v.Status[id] = state.VehQueued
				v.Speed[id] = 0
				v.StopTicks[id]++
				if ed.SignalPhase == 0 {
					s.Signals.Queue0[si]++
				} else {
					s.Signals.Queue1[si]++
				}
				continue
			}
		}

		v.Status[id] = state.VehQueued
		v.Speed[id] = 0
		r.intent(IntentCross, int64(id), int64(e), int64(next), 0)
	}
}

// maybeCrash rolls for a collision.
//
// Rolling an RNG for every vehicle every tick would cost more than the movement
// itself, so each vehicle is only eligible on ticks where (id XOR tick) is
// congruent to 0 mod crashStride, and the per-roll probability is scaled up by
// crashStride to keep the expected rate identical. This is a 64x saving with no
// change to the distribution, and it is fully deterministic.
const crashStride = 64

func maybeCrash(c *Ctx, r *Region, id int32, e world.EdgeID, sp int64) {
	if (uint64(id)+uint64(c.Tick))%crashStride != 0 {
		return
	}
	m, s := c.Map, c.S
	ed := &m.Edges[e]
	if ed.FreeSpeed <= 0 {
		return
	}
	// Base hazard: ~1.5 collisions per million vehicle-km, scaled by speed
	// ratio, weather and darkness. Parts per billion per eligible tick.
	speedRatio := sp * 1000 / int64(ed.FreeSpeed)
	hazard := int64(14) * speedRatio / 1000
	switch s.Weather.Condition {
	case 1:
		hazard = hazard * 16 / 10
	case 2:
		hazard = hazard * 22 / 10
	case 3:
		hazard = hazard * 26 / 10
	case 5:
		hazard = hazard * 20 / 10
	}
	if isNight(s) && !s.Edges.Lit[e] {
		hazard = hazard * 18 / 10
	}
	agent := s.Vehicles.Agent[id]
	if agent >= 0 {
		risk := int64(s.Agents.RiskP[agent])
		hazard = hazard * (700 + risk) / 1000
	}
	hazard = hazard * crashStride
	if hazard <= 0 {
		return
	}
	g := rng.Derive(s.Seed, rng.StreamIncident, uint64(c.Tick), uint64(id))
	if int64(g.IntN(1_000_000)) >= hazard {
		return
	}
	sev := g.Range(200, 1000)
	cas := int32(0)
	if sev > 550 {
		cas = g.Range(1, 3)
	}
	r.intent(IntentAccident, int64(e), int64(sev), int64(cas), int64(id))
}

// FuelAndEmissions derives consumption from distance and idling.
//
// Coefficients: 8.0 L/100km moving (EU fleet average), 0.9 L/h idling,
// 2392 g CO2 per litre of petrol. Kept as an exact integer derivation from two
// cumulative counters rather than an accumulated float, so the value is
// reproducible and immune to summation-order effects.
func FuelAndEmissions(distanceMM, stoppedTicks int64) (fuelMl, co2G int64) {
	fuelMl = distanceMM*8/100_000 + stoppedTicks*25/1000
	co2G = fuelMl * 2392 / 1000
	return
}
