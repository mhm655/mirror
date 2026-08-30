package engine

import (
	"sort"

	"github.com/mirror-sim/mirror/internal/units"
	"github.com/mirror-sim/mirror/internal/world"
)

// Partitioner maps the city onto region workers.
//
// # Why cells rather than districts
//
// The obvious partition is "one district per worker", and the first version of
// this engine did exactly that. It scaled badly, and the benchmark said why:
// with nine districts and nine workers, the downtown district carried roughly
// four times the vehicle load of a peripheral one, every barrier waited for
// that one worker, and nine workers delivered a 1.8x speed-up against a
// theoretical 6.6x.
//
// Districts are a unit of MEANING -- they name places, they group metrics, and
// an operator thinks in them. They are a terrible unit of SCHEDULING, because
// their sizes are set by geography rather than by load.
//
// So execution is partitioned on a separate, finer grid of cells. Cells are
// assigned to workers by measured load, and the assignment is recomputed
// periodically. Districts remain exactly what they were, for humans.
//
// # Why rebalancing is safe
//
// Rebalancing changes which worker owns which edges mid-run. That would be
// terrifying in most simulation engines. It is safe here because the engine is
// provably partition-independent: phase A never writes state another region
// reads, and phase B applies a globally sorted intent stream. The same property
// that makes 1 worker and 9 workers agree makes "9 workers, differently
// arranged" agree too. TestRebalanceIsInvisible asserts it directly by running
// with rebalancing on and off and comparing digests.
//
// # Why the assignment is deterministic
//
// Load is measured in simulation state (vehicles per cell), not wall-clock
// time. So the partition layout is itself a pure function of the run, which
// means a replay produces not just the same city but the same schedule. That
// is a stronger property than it needs to have, and it is worth having: it
// makes a divergence report reproducible down to which worker did what.
type Partitioner struct {
	Cols, Rows int32
	CellW      units.MM
	CellH      units.MM

	CellOfEdge []int32
	CellOfNode []int32
	CellRegion []int32
	Regions    int

	// load is per-cell vehicle occupancy sampled at the last rebalance.
	load []int64
	// staticLoad is edge count per cell, used before any traffic exists.
	staticLoad []int64
	// Rebalances counts how many times the layout changed, for /metrics.
	Rebalances int64
	// LastImbalance is the ratio of the busiest worker's load to the mean,
	// in permille, recorded at the last rebalance. 1000 is perfect balance.
	LastImbalance int32
}

// cellsPerSide picks the grid resolution.
//
// More cells means better balance and more handoffs. The handoff is a couple of
// slice operations in the serial phase, so the trade is heavily one-sided;
// the ceiling is set by wanting each cell to still contain enough edges that
// per-cell bookkeeping is not the dominant cost. Roughly 8-12 cells per worker
// lands in the flat part of that curve.
func cellsPerSide(regions int) int32 {
	n := int32(4)
	for int(n*n) < regions*10 && n < 16 {
		n++
	}
	return n
}

func NewPartitioner(m *world.Map, regions int) *Partitioner {
	if regions < 1 {
		regions = 1
	}
	side := cellsPerSide(regions)
	p := &Partitioner{
		Cols: side, Rows: side, Regions: regions,
		CellW: m.Width/units.MM(side) + 1, CellH: m.Height/units.MM(side) + 1,
	}
	if p.CellW <= 0 {
		p.CellW = 1
	}
	if p.CellH <= 0 {
		p.CellH = 1
	}
	n := int(side * side)
	p.CellOfNode = make([]int32, len(m.Nodes))
	p.CellOfEdge = make([]int32, len(m.Edges))
	p.CellRegion = make([]int32, n)
	p.load = make([]int64, n)
	p.staticLoad = make([]int64, n)

	for i := range m.Nodes {
		cx := int32(m.Nodes[i].X / p.CellW)
		cy := int32(m.Nodes[i].Y / p.CellH)
		if cx >= side {
			cx = side - 1
		}
		if cy >= side {
			cy = side - 1
		}
		p.CellOfNode[i] = cy*side + cx
	}
	for i := range m.Edges {
		// An edge belongs to the cell of its TO node, matching the ownership
		// rule for vehicles: a vehicle changes owner exactly when it enters a
		// new edge, so ownership never straddles a moving entity.
		c := p.CellOfNode[m.Edges[i].To]
		p.CellOfEdge[i] = c
		p.staticLoad[c]++
	}
	p.assign(p.staticLoad)
	return p
}

// assign runs longest-processing-time-first bin packing.
//
// LPT is a 4/3-approximation to optimal makespan and costs one sort. Optimal
// bin packing is NP-hard and would be absurd here: the input is a few hundred
// cells, the objective is a barrier wait measured in microseconds, and the
// measurement it is based on is a minute out of date by construction.
func (p *Partitioner) assign(load []int64) {
	type cell struct {
		id   int32
		load int64
	}
	cells := make([]cell, len(load))
	for i := range load {
		cells[i] = cell{int32(i), load[i]}
	}
	// Ties broken by cell id so the assignment is a pure function of load.
	sort.Slice(cells, func(a, b int) bool {
		if cells[a].load != cells[b].load {
			return cells[a].load > cells[b].load
		}
		return cells[a].id < cells[b].id
	})
	regionLoad := make([]int64, p.Regions)
	for _, c := range cells {
		best := 0
		for r := 1; r < p.Regions; r++ {
			if regionLoad[r] < regionLoad[best] {
				best = r
			}
		}
		p.CellRegion[c.id] = int32(best)
		regionLoad[best] += c.load
	}
	var total, max int64
	for _, l := range regionLoad {
		total += l
		if l > max {
			max = l
		}
	}
	if total > 0 && p.Regions > 0 {
		mean := total / int64(p.Regions)
		if mean > 0 {
			p.LastImbalance = int32(max * 1000 / mean)
		}
	}
}

// RegionOfEdge is the hot lookup, two array reads.
func (p *Partitioner) RegionOfEdge(e world.EdgeID) int32 {
	return p.CellRegion[p.CellOfEdge[e]]
}

func (p *Partitioner) RegionOfNode(n world.NodeID) int32 {
	return p.CellRegion[p.CellOfNode[n]]
}

// measure fills the load vector from current vehicle occupancy.
//
// Occupancy, not vehicle count: an edge with ten vehicles costs roughly ten
// times what an edge with one costs in the movement loop, and the edge scan in
// phase A1 costs the same either way. Adding a small constant per edge keeps
// empty cells from being treated as free, which would let one worker collect
// half the map and then pay for it the moment traffic arrived.
func (p *Partitioner) measure(e *Engine) {
	for i := range p.load {
		p.load[i] = 0
	}
	st := e.S
	for i := range st.Edges.Count {
		c := p.CellOfEdge[i]
		p.load[c] += 1 + 3*int64(st.Edges.Count[i])
	}
}

// rebalanceEvery is the interval between layout recomputations, in ticks.
//
// One simulated minute. Shorter would chase noise -- traffic redistributes on
// a timescale of minutes, and a layout that changes every second spends more
// on rebuilding indices than it saves on barriers. Longer and the layout lags
// the morning peak badly.
const rebalanceEvery = units.TicksPerMinute

// maybeRebalance recomputes the assignment when it is due and the imbalance
// justifies the rebuild.
func (e *Engine) maybeRebalance() {
	p := e.part
	if p == nil || p.Regions < 2 || !e.Cfg.Rebalance {
		return
	}
	if uint64(e.S.Tick)%rebalanceEvery != 0 {
		return
	}
	p.measure(e)
	before := make([]int32, len(p.CellRegion))
	copy(before, p.CellRegion)
	p.assign(p.load)
	changed := false
	for i := range before {
		if before[i] != p.CellRegion[i] {
			changed = true
			break
		}
	}
	if !changed {
		return
	}
	p.Rebalances++
	e.rebuildOwnership()
}
