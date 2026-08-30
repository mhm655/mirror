package world

import (
	"fmt"
	"hash/fnv"
	"sort"

	"github.com/mirror-sim/mirror/internal/rng"
	"github.com/mirror-sim/mirror/internal/units"
)

// GenParams describes a procedurally generated city.
//
// Procedural generation rather than real OSM data, for v1, because: the map is
// an *input* to a determinism contract, and a generated map is exactly
// reproducible from 40 bytes of parameters, whereas an OSM extract is a
// multi-hundred-megabyte artefact that must be content-addressed and shipped
// alongside every replay. The Map type is deliberately agnostic -- an OSM
// importer that produces the same struct is a strictly additive change
// (see docs/adr/ADR-009-map-source.md).
type GenParams struct {
	Name             string
	Seed             uint64
	Blocks           int32 // nodes per side
	BlockSize        units.MM
	DistrictsPerSide int32
	ArterialEvery    int32
	// PruneLocalPermille of local links are removed to break grid regularity.
	PruneLocalPermille int32
}

func DefaultParams(preset string, seed uint64) GenParams {
	p := GenParams{Name: preset, Seed: seed, ArterialEvery: 4, PruneLocalPermille: 120, DistrictsPerSide: 3}
	switch preset {
	case "small":
		p.Blocks, p.BlockSize = 21, 200*units.Metre
		p.DistrictsPerSide = 2
	case "large":
		p.Blocks, p.BlockSize = 81, 150*units.Metre
		p.DistrictsPerSide = 4
	case "huge":
		p.Blocks, p.BlockSize = 141, 150*units.Metre
		p.DistrictsPerSide = 5
	default: // medium
		p.Name = "medium"
		p.Blocks, p.BlockSize = 45, 180*units.Metre
		p.DistrictsPerSide = 3
	}
	return p
}

var districtNames = []string{
	"Harborline", "Oldgate", "Kessler", "Northfield", "Vantage", "Ironmarket",
	"Calder", "Sablewood", "Ridgeway", "Brightwater", "Merridale", "Stonecross",
	"Aldergrove", "Fenwick", "Lowry", "Marchmont", "Quarry Hill", "Templeton",
	"Ashcombe", "Grayling", "Hollowfield", "Winterbourne", "Larkspur", "Dunmore",
	"Eastvale",
}

// Generate builds a city. Pure function of GenParams: same params -> byte
// identical Map (verified by TestMapHashStable).
func Generate(p GenParams) *Map {
	if p.Blocks < 5 {
		p.Blocks = 5
	}
	cols, rows := p.Blocks, p.Blocks
	m := &Map{
		Name:   p.Name,
		Seed:   p.Seed,
		Width:  units.MM(cols-1) * p.BlockSize,
		Height: units.MM(rows-1) * p.BlockSize,
	}
	cx, cy := m.Width/2, m.Height/2

	// ---- districts -------------------------------------------------------
	dps := p.DistrictsPerSide
	dw, dh := m.Width/units.MM(dps), m.Height/units.MM(dps)
	for dy := int32(0); dy < dps; dy++ {
		for dx := int32(0); dx < dps; dx++ {
			id := DistrictID(dy*dps + dx)
			d := District{
				ID:   id,
				Name: districtNames[int(id)%len(districtNames)],
				MinX: units.MM(dx) * dw, MinY: units.MM(dy) * dh,
				MaxX: units.MM(dx+1) * dw, MaxY: units.MM(dy+1) * dh,
			}
			if dx == dps-1 {
				d.MaxX = m.Width + 1
			}
			if dy == dps-1 {
				d.MaxY = m.Height + 1
			}
			d.CX, d.CY = (d.MinX+d.MaxX)/2, (d.MinY+d.MaxY)/2
			m.Districts = append(m.Districts, d)
		}
	}
	districtAt := func(x, y units.MM) DistrictID {
		dx := int32(x / dw)
		dy := int32(y / dh)
		if dx >= dps {
			dx = dps - 1
		}
		if dy >= dps {
			dy = dps - 1
		}
		return DistrictID(dy*dps + dx)
	}

	// ---- nodes -----------------------------------------------------------
	m.Nodes = make([]Node, 0, int(cols*rows))
	for r := int32(0); r < rows; r++ {
		for c := int32(0); c < cols; c++ {
			x, y := units.MM(c)*p.BlockSize, units.MM(r)*p.BlockSize
			m.Nodes = append(m.Nodes, Node{X: x, Y: y, District: districtAt(x, y), Signal: -1})
		}
	}
	nid := func(r, c int32) NodeID { return NodeID(r*cols + c) }

	isArt := func(i int32) bool { return i%p.ArterialEvery == 0 }
	isRing := func(i int32) bool { return i == 0 || i == cols-1 }

	// ---- edges -----------------------------------------------------------
	g := rng.Derive(p.Seed, rng.StreamWorldGen, 0, 0)
	type pair struct{ a, b NodeID }
	var raw []pair
	addPair := func(a, b NodeID) { raw = append(raw, pair{a, b}) }
	for r := int32(0); r < rows; r++ {
		for c := int32(0); c < cols; c++ {
			if c+1 < cols {
				if keepLink(&g, isArt(r) || isRing(r), p.PruneLocalPermille) {
					addPair(nid(r, c), nid(r, c+1))
				}
			}
			if r+1 < rows {
				if keepLink(&g, isArt(c) || isRing(c), p.PruneLocalPermille) {
					addPair(nid(r, c), nid(r+1, c))
				}
			}
		}
	}
	// Connectivity is guaranteed structurally: the arterial and ring skeleton
	// is never pruned, and every node lies on at least one unpruned arterial
	// line by construction of the grid. TestMapConnected asserts it anyway.

	classOf := func(a, b NodeID) RoadClass {
		ra, ca := int32(a)/cols, int32(a)%cols
		rb, cb := int32(b)/cols, int32(b)%cols
		if ra == rb { // horizontal
			if isRing(ra) {
				return ClassMotorway
			}
			if isArt(ra) {
				return ClassArterial
			}
		} else {
			if isRing(ca) {
				return ClassMotorway
			}
			if isArt(ca) {
				return ClassArterial
			}
		}
		// Collectors are the streets adjacent to an arterial.
		if isArt(ra-1) || isArt(rb+1) || isArt(ca-1) || isArt(cb+1) {
			return ClassCollector
		}
		return ClassLocal
	}

	speedOf := [...]int64{40, 50, 60, 90} // km/h by class
	lanesOf := [...]int32{1, 1, 2, 3}

	mkEdge := func(a, b NodeID) {
		cl := classOf(a, b)
		na, nb := &m.Nodes[a], &m.Nodes[b]
		dx, dy := int64(nb.X-na.X), int64(nb.Y-na.Y)
		length := units.MM(units.ISqrt(dx*dx + dy*dy))
		lanes := lanesOf[cl]
		jam := int32(length/(7*units.Metre)) * lanes
		if jam < 1 {
			jam = 1
		}
		m.Edges = append(m.Edges, Edge{
			From: a, To: b, Length: length,
			FreeSpeed: units.KmhToMMPerTick(speedOf[cl]),
			Lanes:     lanes, Jam: jam, Class: cl,
			District: m.Nodes[b].District, SignalPhase: -1, Feeder: -1,
		})
	}
	for _, pr := range raw {
		mkEdge(pr.a, pr.b)
		mkEdge(pr.b, pr.a)
	}

	buildCSR(m)

	// ---- signals ---------------------------------------------------------
	// A signal goes where two roads of arterial class or better cross with
	// degree >= 3. Phase 0 = east/west, phase 1 = north/south.
	for n := range m.Nodes {
		r, c := int32(n)/cols, int32(n)%cols
		major := (isArt(r) || isRing(r)) && (isArt(c) || isRing(c))
		if !major || len(m.Out(NodeID(n))) < 3 {
			continue
		}
		green := int32(22 * units.TicksPerSecond)
		if isRing(r) || isRing(c) {
			green = int32(30 * units.TicksPerSecond)
		}
		sig := Signal{
			Node: NodeID(n), NumPhases: 2,
			PhaseTicks: [4]int32{green, green, 0, 0},
			// Offset staggers adjacent signals to create a green wave along
			// arterials: a signal one block further east turns green one
			// block-traversal later.
			Offset: (c * int32(int64(p.BlockSize)/int64(units.KmhToMMPerTick(50)))) % (2 * green),
			Feeder: -1,
		}
		m.Nodes[n].Signal = int32(len(m.Signals))
		m.Signals = append(m.Signals, sig)
	}
	// Assign each incoming edge to a phase based on its orientation.
	for e := range m.Edges {
		ed := &m.Edges[e]
		if m.Nodes[ed.To].Signal < 0 {
			continue
		}
		na, nb := &m.Nodes[ed.From], &m.Nodes[ed.To]
		if abs64(int64(nb.X-na.X)) >= abs64(int64(nb.Y-na.Y)) {
			ed.SignalPhase = 0
		} else {
			ed.SignalPhase = 1
		}
	}

	// ---- land use --------------------------------------------------------
	genPOIs(m, &g, cx, cy)
	genInfrastructure(m, &g, cols, rows, p)
	genTransit(m, cols, rows, p)

	m.ReverseEdge = make([]EdgeID, len(m.Edges))
	for i := range m.ReverseEdge {
		m.ReverseEdge[i] = NoEdge
	}
	for i := range m.Edges {
		m.ReverseEdge[i] = edgeBetween(m, m.Edges[i].To, m.Edges[i].From)
	}
	m.RouteStopBase = make([]int32, len(m.Routes))
	var totalStops int32
	for i := range m.Routes {
		m.RouteStopBase[i] = totalStops
		totalStops += int32(len(m.Routes[i].Stops))
	}
	m.TotalStops = totalStops
	m.MaxSpeed = m.MaxFreeSpeed()
	m.Grid = BuildSpatialGrid(m.Nodes, m.Width, m.Height, 4*p.BlockSize)
	indexPOIs(m)
	m.Hash = hashMap(m)
	return m
}

func keepLink(g *rng.PCG32, protected bool, prune int32) bool {
	// A draw is consumed unconditionally so that changing which links are
	// protected cannot shift the random stream for every subsequent link.
	roll := g.Permille()
	if protected {
		return true
	}
	return roll >= prune
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func buildCSR(m *Map) {
	outCount := make([]int32, len(m.Nodes)+1)
	inCount := make([]int32, len(m.Nodes)+1)
	for i := range m.Edges {
		outCount[m.Edges[i].From+1]++
		inCount[m.Edges[i].To+1]++
	}
	for i := 1; i < len(outCount); i++ {
		outCount[i] += outCount[i-1]
		inCount[i] += inCount[i-1]
	}
	m.OutEdges = make([]EdgeID, len(m.Edges))
	m.InEdges = make([]EdgeID, len(m.Edges))
	oc := make([]int32, len(m.Nodes))
	ic := make([]int32, len(m.Nodes))
	for i := range m.Edges {
		f, t := m.Edges[i].From, m.Edges[i].To
		m.OutEdges[outCount[f]+oc[f]] = EdgeID(i)
		oc[f]++
		m.InEdges[inCount[t]+ic[t]] = EdgeID(i)
		ic[t]++
	}
	for n := range m.Nodes {
		m.Nodes[n].OutStart, m.Nodes[n].OutEnd = outCount[n], outCount[n+1]
		m.Nodes[n].InStart, m.Nodes[n].InEnd = inCount[n], inCount[n+1]
		s := m.OutEdges[outCount[n]:outCount[n+1]]
		sort.Slice(s, func(a, b int) bool { return s[a] < s[b] })
		s2 := m.InEdges[inCount[n]:inCount[n+1]]
		sort.Slice(s2, func(a, b int) bool { return s2[a] < s2[b] })
	}
}

func genPOIs(m *Map, g *rng.PCG32, cx, cy units.MM) {
	maxD := units.ISqrt(int64(cx)*int64(cx) + int64(cy)*int64(cy))
	if maxD == 0 {
		maxD = 1
	}
	for n := range m.Nodes {
		nd := &m.Nodes[n]
		dx, dy := int64(nd.X-cx), int64(nd.Y-cy)
		d := units.ISqrt(dx*dx + dy*dy)
		// Centrality in [0,1000]: 1000 downtown, 0 at the city edge.
		centrality := int32(1000 - (d*1000)/maxD)
		if centrality < 0 {
			centrality = 0
		}
		// Jobs cluster downtown, homes cluster outward. Both are non-zero
		// everywhere: a pure dormitory suburb produces an unrealistically
		// clean tidal flow that hides all the interesting cross-traffic.
		jobs := 6 + (centrality*90)/1000 + g.IntN(12)
		homes := 10 + ((1000-centrality)*70)/1000 + g.IntN(20)
		m.POIs = append(m.POIs, POI{
			ID: POIID(len(m.POIs)), Kind: POIHome, Node: NodeID(n),
			X: nd.X, Y: nd.Y, District: nd.District, Capacity: homes, Feeder: -1,
			Name: fmt.Sprintf("Residence block %d", n),
		})
		m.POIs = append(m.POIs, POI{
			ID: POIID(len(m.POIs)), Kind: POIWork, Node: NodeID(n),
			X: nd.X, Y: nd.Y, District: nd.District, Capacity: jobs, Feeder: -1,
			Name: fmt.Sprintf("Workplace %d", n),
		})
		if g.Chance(90) {
			m.POIs = append(m.POIs, POI{
				ID: POIID(len(m.POIs)), Kind: POISchool, Node: NodeID(n),
				X: nd.X, Y: nd.Y, District: nd.District,
				Capacity: 200 + g.IntN(400), Feeder: -1,
				Name: fmt.Sprintf("School %d", n),
			})
		}
		if g.Chance(220) {
			m.POIs = append(m.POIs, POI{
				ID: POIID(len(m.POIs)), Kind: POIBusiness, Node: NodeID(n),
				X: nd.X, Y: nd.Y, District: nd.District,
				Capacity: 20 + g.IntN(120), Feeder: -1,
				Name: fmt.Sprintf("Business %d", n),
			})
		}
	}
}

func genInfrastructure(m *Map, g *rng.PCG32, cols, rows int32, p GenParams) {
	// One substation per ~1.2km square.
	step := int32(1200*units.Metre/p.BlockSize) + 1
	if step < 3 {
		step = 3
	}
	for r := step / 2; r < rows; r += step {
		for c := step / 2; c < cols; c += step {
			n := NodeID(r*cols + c)
			nd := &m.Nodes[n]
			m.Substations = append(m.Substations, Substation{
				ID:   SubstationID(len(m.Substations)),
				Name: fmt.Sprintf("SS-%02d", len(m.Substations)+1),
				X:    nd.X, Y: nd.Y, District: nd.District,
				CapacityKW: 9000 + g.IntN(4000),
			})
		}
	}
	if len(m.Substations) == 0 {
		nd := &m.Nodes[0]
		m.Substations = append(m.Substations, Substation{ID: 0, Name: "SS-01", X: nd.X, Y: nd.Y, CapacityKW: 12000})
	}
	nearestSS := func(x, y units.MM) SubstationID {
		best := SubstationID(0)
		var bd int64 = 1<<62 - 1
		for i := range m.Substations {
			dx, dy := int64(m.Substations[i].X-x), int64(m.Substations[i].Y-y)
			d := dx*dx + dy*dy
			if d < bd {
				bd, best = d, m.Substations[i].ID
			}
		}
		return best
	}
	// Neighbour links for load shedding: substations within 2.5km.
	for i := range m.Substations {
		for j := range m.Substations {
			if i == j {
				continue
			}
			dx := int64(m.Substations[i].X - m.Substations[j].X)
			dy := int64(m.Substations[i].Y - m.Substations[j].Y)
			if units.ISqrt(dx*dx+dy*dy) <= int64(2500*units.Metre) {
				m.Substations[i].Neighbours = append(m.Substations[i].Neighbours, SubstationID(j))
			}
		}
	}
	for i := range m.POIs {
		m.POIs[i].Feeder = nearestSS(m.POIs[i].X, m.POIs[i].Y)
	}
	for i := range m.Signals {
		nd := &m.Nodes[m.Signals[i].Node]
		m.Signals[i].Feeder = nearestSS(nd.X, nd.Y)
	}
	for i := range m.Edges {
		nd := &m.Nodes[m.Edges[i].To]
		m.Edges[i].Feeder = nearestSS(nd.X, nd.Y)
	}

	// Connected load per substation. Coefficients are per-unit-capacity:
	// 1.1 kW per dwelling, 0.9 kW per job, 0.6 kW per school place,
	// 2.5 kW per hospital bed (hospitals are extraordinarily power dense),
	// 1.4 kW per business unit.
	loadPerUnit := [...]int32{11, 9, 6, 25, 14, 5}
	for i := range m.POIs {
		p := &m.POIs[i]
		if p.Feeder >= 0 {
			m.Substations[p.Feeder].BaseKW += p.Capacity * loadPerUnit[p.Kind] / 10
		}
	}
	for i := range m.Signals {
		if m.Signals[i].Feeder >= 0 {
			m.Substations[m.Signals[i].Feeder].BaseKW += 2
		}
	}

	// Hospitals.
	hstep := int32(2200*units.Metre/p.BlockSize) + 1
	for r := hstep / 2; r < rows; r += hstep {
		for c := hstep / 2; c < cols; c += hstep {
			n := NodeID(r*cols + c)
			nd := &m.Nodes[n]
			id := HospitalID(len(m.Hospitals))
			m.Hospitals = append(m.Hospitals, Hospital{
				ID: id, Name: fmt.Sprintf("%s General", m.Districts[nd.District].Name),
				Node: n, X: nd.X, Y: nd.Y, District: nd.District,
				Beds: 120 + g.IntN(180), ERBays: 8 + g.IntN(10),
				Ambulances: 3 + g.IntN(4),
				Feeder:     nearestSS(nd.X, nd.Y), BackupMinutes: 240,
			})
			m.POIs = append(m.POIs, POI{
				ID: POIID(len(m.POIs)), Kind: POIHospital, Node: n, X: nd.X, Y: nd.Y,
				District: nd.District, Capacity: m.Hospitals[id].Beds,
				Feeder: m.Hospitals[id].Feeder, Name: m.Hospitals[id].Name,
			})
		}
	}
	// Emergency depots.
	dstep := int32(2600*units.Metre/p.BlockSize) + 1
	kind := uint8(0)
	names := [...]string{"Fire Station", "Police Station", "Ambulance Base"}
	for r := dstep/2 + 1; r < rows; r += dstep {
		for c := dstep/2 + 1; c < cols; c += dstep {
			n := NodeID(r*cols + c)
			nd := &m.Nodes[n]
			m.Depots = append(m.Depots, Depot{
				ID: DepotID(len(m.Depots)), Kind: kind,
				Name: fmt.Sprintf("%s %d", names[kind], len(m.Depots)+1),
				Node: n, X: nd.X, Y: nd.Y, District: nd.District,
				Units: 2 + g.IntN(3),
			})
			kind = (kind + 1) % 3
		}
	}
	// Comms towers.
	tstep := int32(1500*units.Metre/p.BlockSize) + 1
	for r := int32(1); r < rows; r += tstep {
		for c := int32(1); c < cols; c += tstep {
			n := NodeID(r*cols + c)
			nd := &m.Nodes[n]
			m.Towers = append(m.Towers, Tower{
				ID: TowerID(len(m.Towers)), Node: n, X: nd.X, Y: nd.Y,
				District: nd.District, CapacityErl: 1200 + g.IntN(800),
				Feeder: nearestSS(nd.X, nd.Y), BatteryMin: 120,
			})
		}
	}
}

func edgeBetween(m *Map, a, b NodeID) EdgeID {
	for _, e := range m.Out(a) {
		if m.Edges[e].To == b {
			return e
		}
	}
	return NoEdge
}

func genTransit(m *Map, cols, rows int32, p GenParams) {
	mk := func(name string, mode uint8, path []NodeID, stopEvery int32, headwaySec, capacity, veh int32) {
		if len(path) < 2 {
			return
		}
		rt := TransitRoute{
			ID: RouteID(len(m.Routes)), Name: name, Mode: mode,
			HeadwayTick: headwaySec * units.TicksPerSecond, Capacity: capacity, Vehicles: veh,
		}
		var stops []NodeID
		var stopIdx []int32
		for i := int32(0); i < int32(len(path)); i += stopEvery {
			stops = append(stops, path[i])
			stopIdx = append(stopIdx, i)
		}
		last := int32(len(path) - 1)
		if stopIdx[len(stopIdx)-1] != last {
			stops = append(stops, path[last])
			stopIdx = append(stopIdx, last)
		}
		for i := 0; i+1 < len(stops); i++ {
			var leg []EdgeID
			for k := stopIdx[i]; k < stopIdx[i+1]; k++ {
				e := edgeBetween(m, path[k], path[k+1])
				if e == NoEdge {
					return // route not realisable on this map; drop it
				}
				leg = append(leg, e)
			}
			rt.Legs = append(rt.Legs, leg)
		}
		rt.Stops = stops
		m.Routes = append(m.Routes, rt)
	}
	for r := int32(0); r < rows; r += p.ArterialEvery * 2 {
		path := make([]NodeID, 0, cols)
		for c := int32(0); c < cols; c++ {
			path = append(path, NodeID(r*cols+c))
		}
		mk(fmt.Sprintf("Bus %d0 East-West", r/p.ArterialEvery+1), 0, path, 4, 480, 60, 6)
	}
	for c := int32(0); c < cols; c += p.ArterialEvery * 2 {
		path := make([]NodeID, 0, rows)
		for r := int32(0); r < rows; r++ {
			path = append(path, NodeID(r*cols+c))
		}
		mk(fmt.Sprintf("Bus %d5 North-South", c/p.ArterialEvery+1), 0, path, 4, 540, 60, 5)
	}
	// One metro line across the middle: faster, higher capacity, and immune to
	// road congestion because it has its own right of way. This is what makes
	// the "add transit capacity" counterfactual interesting.
	midr := (rows / 2) / p.ArterialEvery * p.ArterialEvery
	path := make([]NodeID, 0, cols)
	for c := int32(0); c < cols; c++ {
		path = append(path, NodeID(midr*cols+c))
	}
	mk("Metro Line 1", 1, path, 6, 300, 400, 8)
}

func indexPOIs(m *Map) {
	m.HomesByDistrict = make([][]POIID, len(m.Districts))
	m.WorksByDistrict = make([][]POIID, len(m.Districts))
	for i := range m.POIs {
		p := &m.POIs[i]
		switch p.Kind {
		case POIHome:
			m.HomesByDistrict[p.District] = append(m.HomesByDistrict[p.District], p.ID)
		case POIWork, POIBusiness:
			m.WorksByDistrict[p.District] = append(m.WorksByDistrict[p.District], p.ID)
		}
	}
}

func hashMap(m *Map) uint64 {
	h := fnv.New64a()
	var buf [8]byte
	put := func(v int64) {
		for i := 0; i < 8; i++ {
			buf[i] = byte(v >> (8 * i))
		}
		h.Write(buf[:])
	}
	put(int64(m.Width))
	put(int64(m.Height))
	put(int64(len(m.Nodes)))
	for i := range m.Nodes {
		n := &m.Nodes[i]
		put(int64(n.X))
		put(int64(n.Y))
		put(int64(n.District))
		put(int64(n.Signal))
	}
	put(int64(len(m.Edges)))
	for i := range m.Edges {
		e := &m.Edges[i]
		put(int64(e.From))
		put(int64(e.To))
		put(int64(e.Length))
		put(int64(e.FreeSpeed))
		put(int64(e.Lanes))
		put(int64(e.Jam))
		put(int64(e.Class))
		put(int64(e.SignalPhase))
		put(int64(e.Feeder))
	}
	put(int64(len(m.POIs)))
	for i := range m.POIs {
		p := &m.POIs[i]
		put(int64(p.Kind))
		put(int64(p.Node))
		put(int64(p.Capacity))
		put(int64(p.Feeder))
	}
	put(int64(len(m.Signals)))
	for i := range m.Signals {
		s := &m.Signals[i]
		put(int64(s.Node))
		put(int64(s.NumPhases))
		put(int64(s.Offset))
		for _, t := range s.PhaseTicks {
			put(int64(t))
		}
	}
	put(int64(len(m.Substations)))
	put(int64(len(m.Hospitals)))
	put(int64(len(m.Depots)))
	put(int64(len(m.Towers)))
	put(int64(len(m.Routes)))
	return h.Sum64()
}
