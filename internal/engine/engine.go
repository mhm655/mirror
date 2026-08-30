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
}

func DefaultConfig() Config {
	return Config{
		Preset: "medium", Seed: 20260830, Population: 40000,
		StartHour: 7, Regions: 0, Workers: 0, EventRing: 65536,
	}
}

// TickStats are per-tick engine telemetry. Not part of simulation state:
// timings vary between runs and must never influence a result.
type TickStats struct {
	Tick          units.Tick
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
}

// New creates and seeds a simulation.
func New(cfg Config) *Engine {
	if cfg.EventRing <= 0 {
		cfg.EventRing = 65536
	}
	m := world.Generate(world.DefaultParams(cfg.Preset, cfg.Seed))
	if cfg.Regions <= 0 || cfg.Regions > len(m.Districts) {
		cfg.Regions = len(m.Districts)
	}
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
	if cfg.Regions <= 0 || cfg.Regions > len(m.Districts) {
		cfg.Regions = len(m.Districts)
	}
	if cfg.Workers <= 0 {
		cfg.Workers = runtime.GOMAXPROCS(0)
	}
	e := &Engine{Map: m, S: s, Log: log, Ring: events.NewRing(cfg.EventRing), Cfg: cfg}
	e.buildTopology()
	return e, nil
}

// buildTopology assigns districts to regions and rebuilds every derived index.
//
// Districts are assigned round-robin to regions rather than by contiguity.
// That looks wrong at first glance -- surely neighbouring districts should
// share a worker to minimise handoffs? -- but handoff cost here is a few
// pointer updates in the serial phase, whereas *load* imbalance costs a whole
// tick: with contiguous assignment, the region containing downtown does 4x the
// work of the others and every barrier waits for it. Interleaving spreads the
// dense centre across workers. When the partitioner becomes adaptive (see
// docs/architecture/distributed-execution.md), this is the function that
// changes and nothing else.
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

	districtRegion := make([]int32, len(m.Districts))
	for d := range m.Districts {
		districtRegion[d] = int32(d % n)
	}

	e.regionOfEdge = make([]int32, len(m.Edges))
	for i := range m.Edges {
		r := districtRegion[m.Edges[i].District]
		e.regionOfEdge[i] = r
		e.regions[r].Edges = append(e.regions[r].Edges, world.EdgeID(i))
	}
	for i := range m.Nodes {
		r := districtRegion[m.Nodes[i].District]
		e.regions[r].Nodes = append(e.regions[r].Nodes, world.NodeID(i))
	}
	for i := range m.Signals {
		r := districtRegion[m.Nodes[m.Signals[i].Node].District]
		e.regions[r].Signals = append(e.regions[r].Signals, int32(i))
	}

	na := e.S.Agents.Len()
	e.regionOfAgent = make([]int32, na)
	minutes := int(units.TicksPerDay / units.TicksPerMinute)
	for i := 0; i < na; i++ {
		r := districtRegion[e.S.Agents.District[i]]
		e.regionOfAgent[i] = r
		reg := e.regions[r]
		reg.Agents = append(reg.Agents, int32(i))
		out := int(e.S.Agents.DepartOut[i]) / units.TicksPerMinute
		ret := int(e.S.Agents.DepartRet[i]) / units.TicksPerMinute
		if out >= 0 && out < minutes {
			reg.DepartOut[out] = append(reg.DepartOut[out], int32(i))
		}
		if ret >= 0 && ret < minutes {
			reg.DepartRet[ret] = append(reg.DepartRet[ret], int32(i))
		}
	}

	// Rebuild live vehicle and walker membership from state.
	e.vehRegion = make([]int32, e.S.Vehicles.Len())
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
		r := e.regionOfEdge[ed]
		e.vehRegion[i] = r
		e.regions[r].AddVehicle(int32(i))
	}
	e.walkRegion = make([]int32, na)
	for i := range e.walkRegion {
		e.walkRegion[i] = -1
	}
	for i := 0; i < na; i++ {
		// WalkTicks carries an explicit sentinel (systems.NotWalking), so the
		// walker set is read directly out of state rather than inferred.
		if e.S.Agents.WalkTicks[i] >= 0 {
			r := e.regionOfAgent[i]
			e.walkRegion[i] = r
			e.regions[r].AddWalker(int32(i))
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

	e.global.Reset()
	for i := range e.regions {
		e.regions[i].Reset()
	}

	// 0. commands
	for _, cmd := range e.Log.At(e.S.Tick) {
		systems.ApplyCommand(c, e.global, e.idx, cmd)
	}

	// 1. city-wide systems
	systems.WeatherSystem(c, e.global)
	systems.PowerSystem(c, e.global, e.idx)
	systems.CommsSystem(c, e.global)
	systems.HospitalSystem(c, e.global)
	systems.IncidentSystem(c, e.global, e.idx, e.disp)
	systems.TransitDispatch(c, e.global)

	// 2 & 3. parallel phases with a barrier between them
	ta := time.Now()
	e.runPhase(func(r *systems.Region) {
		systems.UpdateEdgeSpeeds(c, r)
		systems.UpdateSignals(c, r)
	})
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
	active := systems.ActiveVehicleCount(e.S)
	systems.SampleMetrics(c, active)

	e.S.Tick++

	total := time.Since(t0).Nanoseconds()
	e.recent[e.recentIdx&255] = total
	e.recentIdx++
	sp := int32(0)
	if total > 0 {
		sp = int32(phaseB * 100 / total)
	}
	e.Stat = TickStats{
		Tick: e.S.Tick, PhaseANanos: phaseA, PhaseBNanos: phaseB, TotalNanos: total,
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
