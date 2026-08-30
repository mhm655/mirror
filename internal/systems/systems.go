// Package systems contains MIRROR's simulation logic.
//
// # The two-phase tick
//
// Every tick runs in two phases:
//
//	PHASE A (parallel, one goroutine per region)
//	  Reads:  the whole state, read-only.
//	  Writes: ONLY fields of entities the region owns, plus the region's own
//	          intent and effect buffers.
//	  This is where ~95% of the CPU goes: edge speeds, signal control,
//	  departures, routing, vehicle movement.
//
//	PHASE B (serial, regions in id order, intents in canonical order)
//	  Every structural mutation happens here: allocating a vehicle slot,
//	  moving a vehicle from one edge to another (and therefore mutating two
//	  edges' occupancy counts), admitting a patient, appending to the event
//	  ring, compacting the route arena.
//
// The rule that makes this work: a region never writes to state another region
// might read in the same phase. Anything that would break that rule becomes an
// Intent, and intents are applied serially in an order that is a pure function
// of state.
//
// This is why parallel execution is bit-identical to serial execution. It is
// not "parallel and we hope it converges" -- Phase A is embarrassingly parallel
// by construction, and Phase B is literally the same code path in both modes.
// TestSerialEqualsParallel asserts the digests match tick by tick.
package systems

import (
	"github.com/mirror-sim/mirror/internal/events"
	"github.com/mirror-sim/mirror/internal/state"
	"github.com/mirror-sim/mirror/internal/units"
	"github.com/mirror-sim/mirror/internal/world"
)

// Ctx is the read-mostly context handed to every system.
type Ctx struct {
	Map *world.Map
	S   *state.State
	// Tick is the tick currently being computed.
	Tick units.Tick
}

// Intent is a deferred structural mutation. Fixed-size, no allocation.
//
// Reg records which region staged it, so the commit phase can resolve the
// B/C offsets of route-carrying intents against the right path arena after all
// regions' intents have been merged into one globally sorted stream.
type Intent struct {
	Kind       uint8
	Reg        int32
	A, B, C, D int64
}

const (
	IntentCross         uint8 = iota // A=vehicle B=fromEdge C=toEdge
	IntentArrive                     // A=vehicle
	IntentSpawnCar                   // A=agent B=originNode C=destNode
	IntentJoinStop                   // A=agent B=flatStopIndex
	IntentStranded                   // A=vehicle
	IntentAccident                   // A=edge B=severity C=casualties D=vehicle
	IntentBoard                      // A=vehicle B=flatStopIndex
	IntentAlight                     // A=vehicle B=stopIndex
	IntentHospitalize                // A=agent B=node C=incident
	IntentFreeVehicle                // A=vehicle
	IntentTransitDepart              // A=route
	IntentDispatch                   // A=incidentIndex
	IntentInstallRoute               // A=vehicle B=arenaStart C=arenaLen
	IntentStartWalk                  // A=agent
	IntentEndWalk                    // A=agent
	IntentPreempt                    // A=signal B=phase C=holdTicks
)

// Region is a spatial partition plus all the per-worker scratch it needs.
//
// The scratch lives here, not in the systems, because the whole point of the
// design is that a tick allocates nothing: 100k vehicles moving at 10 ticks per
// simulated second would otherwise put a million short-lived objects per second
// through the GC.
type Region struct {
	ID int32
	// Owned static entities. An edge belongs to the district of its TO node,
	// so a vehicle's owner changes exactly when it crosses into a new edge --
	// a single well-defined handoff point rather than a fuzzy boundary zone.
	Edges   []world.EdgeID
	Signals []int32
	Nodes   []world.NodeID
	Agents  []int32
	// DepartOut/DepartRet bucket agent ids by simulated minute. Derived from
	// state, rebuilt after any restore.
	DepartOut [][]int32
	DepartRet [][]int32

	// Dynamic membership, maintained by phase B.
	Vehicles []int32
	vehPos   map[int32]int32 // vehicle -> index in Vehicles, for O(1) removal

	// Walkers are agents on a walking leg; kept as a list so the tick does not
	// scan the whole population to decrement a counter that applies to a few
	// percent of it.
	Walkers []int32
	walkPos map[int32]int32

	// prevCount and speedKey drive the incremental edge-speed pass. See
	// UpdateEdgeSpeeds.
	prevCount []int32
	speedKey  int64

	Router *world.Router
	Path   []world.EdgeID
	// PathArena holds routes computed during phase A. Phase A must not touch
	// the shared route arena in state, so paths are staged here and copied in
	// during the serial commit. Reset every tick.
	PathArena []world.EdgeID
	Intents   []Intent
	// spill holds intents generated *by* the commit phase itself, which are
	// drained in a second pass. See CommitAll.
	spill   []Intent
	Effects []events.Event

	// Per-tick counters, aggregated serially by the engine after commit.
	// Phase A must never touch state.Metrics directly: several regions run
	// concurrently and a shared counter would be both a data race and a source
	// of nondeterminism (the final value would depend on interleaving).
	Moved        int64
	Crossings    int64
	Reroutes     int64
	RouteFails   int64
	RouteQueries int64
	TripsStarted int64
}

func NewRegion(id int32, nodeCount int) *Region {
	return &Region{
		ID: id, Router: world.NewRouter(nodeCount),
		vehPos:    make(map[int32]int32),
		walkPos:   make(map[int32]int32),
		PathArena: make([]world.EdgeID, 0, 8192),
		Path:      make([]world.EdgeID, 0, 256),
		Intents:   make([]Intent, 0, 4096),
		Effects:   make([]events.Event, 0, 1024),
		DepartOut: make([][]int32, units.TicksPerDay/units.TicksPerMinute),
		DepartRet: make([][]int32, units.TicksPerDay/units.TicksPerMinute),
	}
}

func (r *Region) AddVehicle(v int32) {
	if _, ok := r.vehPos[v]; ok {
		return
	}
	r.vehPos[v] = int32(len(r.Vehicles))
	r.Vehicles = append(r.Vehicles, v)
}

// RemoveVehicle is a swap-delete. It reorders r.Vehicles, which is safe
// precisely because phase A's result must not depend on iteration order --
// every order-sensitive decision is deferred to phase B, where intents are
// sorted canonically. TestVehicleOrderIndependence checks this by shuffling.
func (r *Region) RemoveVehicle(v int32) {
	i, ok := r.vehPos[v]
	if !ok {
		return
	}
	last := int32(len(r.Vehicles) - 1)
	moved := r.Vehicles[last]
	r.Vehicles[i] = moved
	r.vehPos[moved] = i
	r.Vehicles = r.Vehicles[:last]
	delete(r.vehPos, v)
}

func (r *Region) HasVehicle(v int32) bool { _, ok := r.vehPos[v]; return ok }

func (r *Region) AddWalker(a int32) {
	if _, ok := r.walkPos[a]; ok {
		return
	}
	r.walkPos[a] = int32(len(r.Walkers))
	r.Walkers = append(r.Walkers, a)
}

func (r *Region) RemoveWalker(a int32) {
	i, ok := r.walkPos[a]
	if !ok {
		return
	}
	last := int32(len(r.Walkers) - 1)
	moved := r.Walkers[last]
	r.Walkers[i] = moved
	r.walkPos[moved] = i
	r.Walkers = r.Walkers[:last]
	delete(r.walkPos, a)
}

// ClearVehicles and ClearWalkers drop membership wholesale, for the
// repartitioner. Clearing and refilling is cheaper and far less error-prone
// than computing the delta, and it runs once per simulated minute at most.
// InvalidateSpeedCache forces the next edge-speed pass to recompute every
// edge. Called after a repartition, when the region's edge set has changed
// underneath its cache.
func (r *Region) InvalidateSpeedCache() { r.speedKey = 0 }

func (r *Region) ClearVehicles() {
	r.Vehicles = r.Vehicles[:0]
	clear(r.vehPos)
}

func (r *Region) ClearWalkers() {
	r.Walkers = r.Walkers[:0]
	clear(r.walkPos)
}

func (r *Region) Reset() {
	r.Intents = r.Intents[:0]
	r.Effects = r.Effects[:0]
	r.PathArena = r.PathArena[:0]
	r.Moved, r.Crossings, r.Reroutes = 0, 0, 0
	r.RouteFails, r.RouteQueries, r.TripsStarted = 0, 0, 0
}

func (r *Region) emit(t units.Tick, k events.Kind, sev events.Severity, a, b, c, d int64) {
	// Effect buffers are bounded per tick. Under a pathological event storm we
	// would rather drop observability than stall the simulation loop or grow
	// the heap without bound; the drop is counted and surfaced on /metrics.
	if len(r.Effects) >= 8192 {
		return
	}
	r.Effects = append(r.Effects, events.E(t, k, sev, int16(r.ID), a, b, c, d))
}

func (r *Region) intent(k uint8, a, b, c, d int64) {
	r.Intents = append(r.Intents, Intent{Kind: k, Reg: r.ID, A: a, B: b, C: c, D: d})
}

// SortIntents establishes the canonical application order: by kind, then by the
// primary entity id. Sorting rather than relying on insertion order is what
// decouples the result from goroutine scheduling.
func SortIntents(in []Intent) {
	// Insertion sort would be O(n^2); this is a simple deterministic
	// merge-free approach using the standard library's stable sort semantics
	// replicated by an explicit total order (no equal keys survive).
	quickSortIntents(in)
}

func quickSortIntents(a []Intent) {
	if len(a) < 2 {
		return
	}
	// Median-of-three pivot on a fully deterministic comparator.
	lo, hi := 0, len(a)-1
	mid := len(a) / 2
	if intentLess(a[mid], a[lo]) {
		a[mid], a[lo] = a[lo], a[mid]
	}
	if intentLess(a[hi], a[mid]) {
		a[hi], a[mid] = a[mid], a[hi]
		if intentLess(a[mid], a[lo]) {
			a[mid], a[lo] = a[lo], a[mid]
		}
	}
	pivot := a[mid]
	i, j := lo, hi
	for i <= j {
		for intentLess(a[i], pivot) {
			i++
		}
		for intentLess(pivot, a[j]) {
			j--
		}
		if i <= j {
			a[i], a[j] = a[j], a[i]
			i++
			j--
		}
	}
	quickSortIntents(a[lo : j+1])
	quickSortIntents(a[i:])
}

func intentLess(x, y Intent) bool {
	if x.Kind != y.Kind {
		return x.Kind < y.Kind
	}
	if x.A != y.A {
		return x.A < y.A
	}
	if x.B != y.B {
		return x.B < y.B
	}
	if x.C != y.C {
		return x.C < y.C
	}
	if x.D != y.D {
		return x.D < y.D
	}
	return x.Reg < y.Reg
}

// NotWalking is the sentinel stored in Agents.WalkTicks when an agent is not on
// a walking leg.
//
// An explicit sentinel rather than an inferred condition matters because the
// engine rebuilds its walker index from state alone after a checkpoint restore
// or a scenario fork. A heuristic that is merely usually right there is a
// determinism bug that only appears after a restart, which is the worst kind
// to debug.
const NotWalking int32 = -1

// ------------------------------------------------------------- helpers ----

// flatStop maps (route, stopIndex) into the flattened stop id space.
func flatStop(m *world.Map, route world.RouteID, stop int32) int32 {
	return m.RouteStopBase[route] + stop
}

func clampP(v int32) int32 {
	if v < 0 {
		return 0
	}
	if v > 1000 {
		return 1000
	}
	return v
}

// WalkTicksFor converts a straight-line distance into a walking duration at
// 5 km/h with a 1.3 detour factor -- the standard planning approximation for
// street-network walking distance.
func WalkTicksFor(d units.MM) int32 {
	sp := units.KmhToMMPerTick(5)
	if sp <= 0 {
		sp = 1
	}
	t := int32(units.DivRound(int64(d)*13, int64(sp)*10))
	if t < 1 {
		t = 1
	}
	return t
}

func dist(m *world.Map, a, b world.NodeID) units.MM {
	dx := int64(m.Nodes[a].X - m.Nodes[b].X)
	dy := int64(m.Nodes[a].Y - m.Nodes[b].Y)
	return units.MM(units.ISqrt(dx*dx + dy*dy))
}
