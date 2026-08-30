// Package engine orchestrates the simulation: partitioning, the two-phase
// tick, checkpointing and replay.
//
// The engine is the only place that knows about goroutines. Every system in
// internal/systems is a plain function over (Ctx, Region) and has no idea
// whether it is running on one core or twelve.
package engine

import (
	"errors"
	"runtime"
	"sync"
	"time"

	"github.com/mirror-sim/mirror/internal/events"
	"github.com/mirror-sim/mirror/internal/state"
	"github.com/mirror-sim/mirror/internal/systems"
	"github.com/mirror-sim/mirror/internal/units"
	"github.com/mirror-sim/mirror/internal/world"
)

// Config describes a simulation run.
type Config struct {
	Preset     string
	Seed       uint64
	Population int
	StartHour  int32
	// Regions is the number of spatial partitions. 1 means fully serial.
	// Must divide evenly into the map's district count or be equal to it.
	Regions int
	// Workers is the goroutine count for phase A. 0 = GOMAXPROCS.
	Workers int
	// EventRing sizes the effect buffer.
	EventRing int
	// Rebalance enables periodic load-based repartitioning. On by default;
	// the determinism suite runs with it both on and off and requires the
	// digests to match.
	Rebalance bool
}

func DefaultConfig() Config {
	return Config{
		Preset: "medium", Seed: 20260830, Population: 40000,
		StartHour: 7, Regions: 0, Workers: 0, EventRing: 65536, Rebalance: true,
	}
}

// TickStats are per-tick engine telemetry. Not part of simulation state:
// timings vary between runs and must never influence a result.
type TickStats struct {
	Tick          units.Tick
	GlobalNanos   int64
	PhaseA1Nanos  int64
	PhaseANanos   int64
	PhaseBNanos   int64
	TotalNanos    int64
	Intents       int
	Effects       int
	Crossings     int64
	Moved         int64
	ActiveVeh     int32
	RouteQueries  uint64
	RouteExpands  uint64
	SerialPercent int32
}

// Engine is one simulation instance.
type Engine struct {
	Map  *world.Map
	S    *state.State
	Log  *events.Log
	Ring *events.Ring
	Cfg  Config

	regions []*systems.Region
	global  *systems.Region
	idx     *systems.PowerIndex
	disp    *systems.Dispatcher
	part    *Partitioner

	// Derived ownership tables. Rebuilt from state on every load; never
	// checkpointed, because they are a pure function of the map and the
	// population, and storing derived data is how checkpoints rot.
	regionOfEdge  []int32
	regionOfAgent []int32
	vehRegion     []int32
	walkRegion    []int32

	// intents is the merged, globally sorted intent stream for the current
	// tick. Reused across ticks so the commit phase allocates nothing.
	intents []systems.Intent

	wg   sync.WaitGroup
	Stat TickStats
	// PeakTickNanos and a small ring of recent tick durations, for /metrics.
	recent    [256]int64
	recentIdx int
	activeVeh int32
}

// New creates and seeds a simulation, generating its world.
func New(cfg Config) *Engine {
	return NewWithMap(world.Generate(world.DefaultParams(cfg.Preset, cfg.Seed)), cfg)
}

// NewWithMap creates and seeds a simulation on an existing world.
//
// Callers that run many simulations over the same city should go through this
// and share one Map pointer. The map is immutable, so sharing is safe, and it
// is by far the largest single allocation in the process.
func NewWithMap(m *world.Map, cfg Config) *Engine {
	if cfg.EventRing <= 0 {
		cfg.EventRing = 65536
	}
	cfg.Regions = clampRegions(cfg.Regions)
	if cfg.Workers <= 0 {
		cfg.Workers = runtime.GOMAXPROCS(0)
	}
	s := state.NewState(m, cfg.Seed, cfg.Population, cfg.StartHour)
	e := &Engine{Map: m, S: s, Log: &events.Log{}, Ring: events.NewRing(cfg.EventRing), Cfg: cfg}

	// The seeding command goes into the authoritative log so that a replay
	// from tick 0 reproduces the identical population.
	e.Log.Append(events.C(0, events.CmdSeedPopulation, int64(cfg.Population), int64(cfg.StartHour), int64(cfg.Seed), 0))
	systems.SeedPopulation(m, s, cfg.Population, cfg.StartHour)
	e.buildTopology()
	return e
}

// NewFromState rebuilds an engine around an existing state (checkpoint restore
// or scenario fork).
func NewFromState(m *world.Map, s *state.State, log *events.Log, cfg Config) (*Engine, error) {
	if s.MapHash != m.Hash {
		return nil, errors.New("engine: state was produced by a different map")
	}
	if cfg.EventRing <= 0 {
		cfg.EventRing = 65536
	}
	cfg.Regions = clampRegions(cfg.Regions)
	if cfg.Workers <= 0 {
		cfg.Workers = runtime.GOMAXPROCS(0)
	}
	e := &Engine{Map: m, S: s, Log: log, Ring: events.NewRing(cfg.EventRing), Cfg: cfg}
	e.buildTopology()
	// The power index caches the energisation pattern it last propagated. A
	// restored state may have a different one, so the cache starts cold.
	e.idx.Invalidate()
	e.activeVeh = systems.ActiveVehicleCount(s)
	return e, nil
}

// clampRegions bounds the worker count.
//
// The upper bound is not GOMAXPROCS: more regions than cores is a legitimate
// configuration for testing the partitioning, and the benchmark sweeps past it
// deliberately. It is bounded only against absurdity.
func clampRegions(n int) int {
	if n <= 0 {
		n = runtime.GOMAXPROCS(0)
	}
	if n > 64 {
		n = 64
	}
	return n
}

// buildTopology creates the region workers and their derived indices.
func (e *Engine) buildTopology() {
	m := e.Map
	n := e.Cfg.Regions
	e.regions = make([]*systems.Region, n)
	for i := 0; i < n; i++ {
		e.regions[i] = systems.NewRegion(int32(i), len(m.Nodes))
	}
	e.global = systems.NewRegion(-1, len(m.Nodes))
	e.idx = systems.BuildPowerIndex(m)
	e.disp = systems.NewDispatcher(len(m.Nodes))
	e.part = NewPartitioner(m, n)
	e.regionOfEdge = make([]int32, len(m.Edges))
	e.regionOfAgent = make([]int32, e.S.Agents.Len())
	e.rebuildOwnership()
}

// rebuildOwnership recomputes every derived index from the current partition.
//
// Called at startup, after a checkpoint restore, and after each rebalance. It
// is O(edges + agents), which at one call per simulated minute is a rounding
// error, and it is the only place ownership is derived -- so a rebalance and a
// cold start cannot drift apart.
func (e *Engine) rebuildOwnership() {
	m, p := e.Map, e.part

	for _, r := range e.regions {
		r.Edges = r.Edges[:0]
		r.Signals = r.Signals[:0]
		r.Nodes = r.Nodes[:0]
		r.Agents = r.Agents[:0]
		for i := range r.DepartOut {
			r.DepartOut[i] = r.DepartOut[i][:0]
			r.DepartRet[i] = r.DepartRet[i][:0]
		}
		r.ClearVehicles()
		r.ClearWalkers()
		r.InvalidateSpeedCache()
	}

	for i := range m.Edges {
		reg := p.RegionOfEdge(world.EdgeID(i))
		e.regionOfEdge[i] = reg
		e.regions[reg].Edges = append(e.regions[reg].Edges, world.EdgeID(i))
	}
	for i := range m.Nodes {
		reg := p.RegionOfNode(world.NodeID(i))
		e.regions[reg].Nodes = append(e.regions[reg].Nodes, world.NodeID(i))
	}
	for i := range m.Signals {
		// A signal is owned by whoever owns the edges arriving at it, which is
		// the cell of its node -- the same rule, so the ownership of a signal
		// and of every approach to it can never disagree.
		reg := p.RegionOfNode(m.Signals[i].Node)
		e.regions[reg].Signals = append(e.regions[reg].Signals, int32(i))
	}

	na := e.S.Agents.Len()
	if len(e.regionOfAgent) < na {
		e.regionOfAgent = make([]int32, na)
	}
	minutes := int(units.TicksPerDay / units.TicksPerMinute)
	for i := 0; i < na; i++ {
		reg := p.RegionOfNode(e.S.Agents.HomeNode[i])
		e.regionOfAgent[i] = reg
		r := e.regions[reg]
		r.Agents = append(r.Agents, int32(i))
		out := int(e.S.Agents.DepartOut[i]) / units.TicksPerMinute
		ret := int(e.S.Agents.DepartRet[i]) / units.TicksPerMinute
		if out >= 0 && out < minutes {
			r.DepartOut[out] = append(r.DepartOut[out], int32(i))
		}
		if ret >= 0 && ret < minutes {
			r.DepartRet[ret] = append(r.DepartRet[ret], int32(i))
		}
	}

	// Live entities follow their owning edge or home node.
	if len(e.vehRegion) < e.S.Vehicles.Len() {
		e.vehRegion = make([]int32, e.S.Vehicles.Len())
	}
	for i := range e.vehRegion {
		e.vehRegion[i] = -1
	}
	for i := 0; i < e.S.Vehicles.Len(); i++ {
		if e.S.Vehicles.Status[i] == state.VehIdle {
			continue
		}
		ed := e.S.Vehicles.Edge[i]
		if ed < 0 {
			continue
		}
		reg := e.regionOfEdge[ed]
		e.vehRegion[i] = reg
		e.regions[reg].AddVehicle(int32(i))
	}
	if len(e.walkRegion) < na {
		e.walkRegion = make([]int32, na)
	}
	for i := range e.walkRegion {
		e.walkRegion[i] = -1
	}
	for i := 0; i < na; i++ {
		// WalkTicks carries an explicit sentinel (systems.NotWalking), so the
		// walker set is read directly out of state rather than inferred.
		if e.S.Agents.WalkTicks[i] >= 0 {
			reg := e.regionOfAgent[i]
			e.walkRegion[i] = reg
			e.regions[reg].AddWalker(int32(i))
		}
	}
}

// ------------------------------------------------ systems.Topology impl ---

func (e *Engine) RegionOfEdge(ed world.EdgeID) int32 {
	if ed < 0 || int(ed) >= len(e.regionOfEdge) {
		return 0
	}
	return e.regionOfEdge[ed]
}

func (e *Engine) RegionOfAgent(a int32) int32 {
	if a < 0 || int(a) >= len(e.regionOfAgent) {
		return 0
	}
	return e.regionOfAgent[a]
}

func (e *Engine) AttachVehicle(v int32, region int32) {
	for int(v) >= len(e.vehRegion) {
		e.vehRegion = append(e.vehRegion, -1)
	}
	if old := e.vehRegion[v]; old >= 0 && old != region {
		e.regions[old].RemoveVehicle(v)
	}
	if region < 0 || int(region) >= len(e.regions) {
		return
	}
	e.vehRegion[v] = region
	e.regions[region].AddVehicle(v)
}

func (e *Engine) DetachVehicle(v int32) {
	if int(v) >= len(e.vehRegion) {
		return
	}
	if old := e.vehRegion[v]; old >= 0 {
		e.regions[old].RemoveVehicle(v)
	}
	e.vehRegion[v] = -1
}

func (e *Engine) AttachWalker(a int32, region int32) {
	if int(a) >= len(e.walkRegion) || region < 0 || int(region) >= len(e.regions) {
		return
	}
	if old := e.walkRegion[a]; old >= 0 && old != region {
		e.regions[old].RemoveWalker(a)
	}
	e.walkRegion[a] = region
	e.regions[region].AddWalker(a)
}

func (e *Engine) DetachWalker(a int32) {
	if int(a) >= len(e.walkRegion) {
		return
	}
	if old := e.walkRegion[a]; old >= 0 {
		e.regions[old].RemoveWalker(a)
	}
	e.walkRegion[a] = -1
}

// ------------------------------------------------------------- the tick ---

// Tick advances the simulation by exactly one step.
//
// The phase order is fixed and load-bearing. Reordering any two of these lines
// changes results, so the order is part of the replay contract and is
// versioned with the codec.
//
//  0. commands scheduled for this tick
//  1. city-wide serial systems (weather, power, comms, hospitals, incidents,
//     transit dispatch) -- small, cross-cutting, not worth partitioning
//  2. phase A1, parallel: edge speeds and signal control (writes owned edges
//     and signals only)
//     ---- barrier ----
//  3. phase A2, parallel: departures, walking, rerouting, movement (reads the
//     whole network, writes owned entities and intent buffers only)
//     ---- barrier ----
//  4. phase B, serial: apply intents, flush effects, aggregate counters
//  5. arena maintenance and metric sampling
func (e *Engine) Tick() {
	t0 := time.Now()
	c := &systems.Ctx{Map: e.Map, S: e.S, Tick: e.S.Tick}

	e.maybeRebalance()

	e.global.Reset()
	for i := range e.regions {
		e.regions[i].Reset()
	}

	// 0. commands
	for _, cmd := range e.Log.At(e.S.Tick) {
		systems.ApplyCommand(c, e.global, e.idx, cmd)
	}

	// 1. city-wide systems
	tg := time.Now()
	systems.WeatherSystem(c, e.global)
	systems.PowerSystem(c, e.global, e.idx)
	systems.CommsSystem(c, e.global)
	systems.HospitalSystem(c, e.global)
	systems.IncidentSystem(c, e.global, e.idx, e.disp)
	systems.TransitDispatch(c, e.global)
	globalNanos := time.Since(tg).Nanoseconds()

	// 2 & 3. parallel phases with a barrier between them
	ta := time.Now()
	e.runPhase(func(r *systems.Region) {
		systems.UpdateEdgeSpeeds(c, r)
		systems.UpdateSignals(c, r)
	})
	phaseA1 := time.Since(ta).Nanoseconds()
	e.runPhase(func(r *systems.Region) {
		systems.Departures(c, r)
		systems.Walkers(c, r)
		systems.Reroute(c, r)
		systems.MoveVehicles(c, r)
	})
	phaseA := time.Since(ta).Nanoseconds()

	// 4. serial commit.
	//
	// All regions' intents are merged into one stream and sorted globally.
	// Committing region by region would let the region *order* decide who wins
	// a contended link slot, which would make results depend on the partition
	// count -- the exact thing TestParallelRegionCounts forbids.
	tb := time.Now()
	e.intents = e.intents[:0]
	e.intents = append(e.intents, e.global.Intents...)
	for _, r := range e.regions {
		e.intents = append(e.intents, r.Intents...)
	}
	// Effects produced during phase A are flushed before commit so the event
	// stream reads in causal order: what the regions observed, then what the
	// commit decided.
	e.flush(e.global)
	for _, r := range e.regions {
		e.flush(r)
	}
	systems.CommitAll(c, e.intents, e.arena, e.global, e, e.disp)
	e.flush(e.global)
	nIntents := len(e.intents)

	// Aggregate region counters into simulation state, in region order.
	var moved int64
	for _, r := range e.regions {
		e.S.Metrics.TripsStarted += r.TripsStarted
		e.S.Metrics.RouteFailures += r.RouteFails
		e.S.Metrics.RouteQueries += r.RouteQueries
		moved += r.Moved
	}
	e.S.Metrics.TripsStarted += e.global.TripsStarted
	e.S.Metrics.RouteQueries += e.global.RouteQueries
	crossings := e.global.Crossings
	var expands uint64
	for _, r := range e.regions {
		expands += r.Router.Expansions
	}
	phaseB := time.Since(tb).Nanoseconds()

	// 5. maintenance
	e.S.CompactRoutes()
	// The active-vehicle scan is O(vehicle slots) and only the per-minute
	// sample and the UI need it, so it runs on the minute boundary and is
	// cached in between.
	if int64(e.S.Tick)/units.TicksPerMinute != e.S.Metrics.LastSeriesMinute {
		e.activeVeh = systems.ActiveVehicleCount(e.S)
	}
	systems.SampleMetrics(c, e.activeVeh)
	active := e.activeVeh
	serialNanos := globalNanos + time.Since(tb).Nanoseconds()

	e.S.Tick++

	total := time.Since(t0).Nanoseconds()
	e.recent[e.recentIdx&255] = total
	e.recentIdx++
	sp := int32(0)
	if total > 0 {
		// The serial fraction includes the city-wide systems and the
		// maintenance pass, not just the commit. Reporting only the commit
		// would understate it and make the Amdahl prediction in the benchmark
		// harness a lie.
		sp = int32(serialNanos * 100 / total)
	}
	e.Stat = TickStats{
		Tick: e.S.Tick, GlobalNanos: globalNanos, PhaseA1Nanos: phaseA1,
		PhaseANanos: phaseA, PhaseBNanos: phaseB, TotalNanos: total,
		Intents: nIntents, Crossings: crossings, Moved: moved, ActiveVeh: active,
		RouteQueries: uint64(e.S.Metrics.RouteQueries), RouteExpands: expands, SerialPercent: sp,
	}
}

// arena resolves a staged path buffer by region id. Region -1 is the global
// pseudo-region used by the city-wide systems and the command handlers.
func (e *Engine) arena(region int32) []world.EdgeID {
	if region < 0 || int(region) >= len(e.regions) {
		return e.global.PathArena
	}
	return e.regions[region].PathArena
}

// runPhase executes fn over every region, in parallel when configured.
//
// With one region it runs inline, avoiding goroutine and barrier overhead
// entirely -- which matters because the single-region path is what the
// determinism tests use as the reference implementation.
func (e *Engine) runPhase(fn func(*systems.Region)) {
	if len(e.regions) == 1 || e.Cfg.Workers == 1 {
		for _, r := range e.regions {
			fn(r)
		}
		return
	}
	e.wg.Add(len(e.regions))
	for i := range e.regions {
		r := e.regions[i]
		go func() {
			defer e.wg.Done()
			fn(r)
		}()
	}
	e.wg.Wait()
}

func (e *Engine) flush(r *systems.Region) {
	for i := range r.Effects {
		e.Ring.Push(r.Effects[i])
	}
	r.Effects = r.Effects[:0]
}

// Run advances n ticks.
func (e *Engine) Run(n int) {
	for i := 0; i < n; i++ {
		e.Tick()
	}
}

// Inject appends a command to the authoritative log.
//
// Commands are always scheduled at the CURRENT tick or later, never in the
// past: a command in the past would mean the materialised state no longer
// matches the log, and every replay would diverge. The API layer rejects
// past-dated injections rather than silently clamping them, so the operator
// finds out.
func (e *Engine) Inject(ev events.Event) (events.Event, error) {
	if !ev.Kind.IsCommand() {
		return ev, errors.New("engine: not a command")
	}
	if ev.Tick < e.S.Tick {
		return ev, errors.New("engine: cannot inject a command into the past")
	}
	if ev.Tick == 0 {
		ev.Tick = e.S.Tick
	}
	return e.Log.Append(ev), nil
}

// Digest returns the canonical state fingerprint.
func (e *Engine) Digest() uint64 { return e.S.Digest() }

// Regions exposes the partitions for introspection and the chaos lab.
func (e *Engine) Regions() []*systems.Region { return e.regions }

// RecentTickNanos returns the last up-to-256 tick durations, newest last.
func (e *Engine) RecentTickNanos(out []int64) []int64 {
	out = out[:0]
	n := e.recentIdx
	if n > 256 {
		n = 256
	}
	for i := e.recentIdx - n; i < e.recentIdx; i++ {
		out = append(out, e.recent[i&255])
	}
	return out
}

// RegionOfDistrict reports which worker owns a district's centre.
//
// Districts no longer map one-to-one onto workers -- execution is partitioned
// on a separate cell grid -- so this is a display convenience, not an
// ownership fact. It answers "which worker is mostly responsible for this
// place", which is what the map overlay wants.
func (e *Engine) RegionOfDistrict(d world.DistrictID) int32 {
	if int(d) >= len(e.Map.Districts) || e.part == nil {
		return 0
	}
	dd := &e.Map.Districts[d]
	n := e.Map.Grid.Nearest(e.Map.Nodes, dd.CX, dd.CY)
	if n < 0 {
		return 0
	}
	return e.part.RegionOfNode(n)
}

// Partition exposes the partitioner for the API and the chaos lab.
func (e *Engine) Partition() *Partitioner { return e.part }
