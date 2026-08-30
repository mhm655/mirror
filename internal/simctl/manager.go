// Package simctl owns the lifecycle of running simulations: play/pause, speed,
// checkpointing, forking and cross-scenario comparison.
//
// One goroutine per simulation drives its engine. Everything else -- HTTP
// handlers, WebSocket writers, the AI agent's tools -- reads through an
// RWMutex. That is the entire concurrency model at this layer, and it is
// deliberately boring: the interesting concurrency lives inside a tick, where
// it is bounded and testable, not in a web of channels between subsystems.
package simctl

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mirror-sim/mirror/internal/engine"
	"github.com/mirror-sim/mirror/internal/events"
	"github.com/mirror-sim/mirror/internal/state"
	"github.com/mirror-sim/mirror/internal/store"
	"github.com/mirror-sim/mirror/internal/units"
	"github.com/mirror-sim/mirror/internal/world"
)

type RunState int32

const (
	Paused RunState = iota
	Running
	Completed
	Failed
)

var runStateName = [...]string{"paused", "running", "completed", "failed"}

func (r RunState) String() string { return runStateName[r] }

// Sim is one simulation or scenario.
type Sim struct {
	ID         string
	Name       string
	ParentID   string
	BranchTick units.Tick
	Created    time.Time
	Cfg        engine.Config

	mu sync.RWMutex
	E  *engine.Engine

	runState atomic.Int32
	// Speed is the simulated-time multiplier. 1 = real time (10 ticks/sec),
	// 0 or negative = run as fast as the CPU allows.
	speed atomic.Int32
	// StopAtTick halts the runner at a tick; 0 = run indefinitely.
	stopAt atomic.Uint64

	// Wall-clock telemetry, not simulation state.
	ticksRun    atomic.Uint64
	lastTPS     atomic.Int64
	lastErr     atomic.Value // string
	checkpointN atomic.Uint64

	wake   chan struct{}
	closed chan struct{}
	once   sync.Once
}

func (s *Sim) State() RunState     { return RunState(s.runState.Load()) }
func (s *Sim) Speed() int32        { return s.speed.Load() }
func (s *Sim) TicksRun() uint64    { return s.ticksRun.Load() }
func (s *Sim) TPS() int64          { return s.lastTPS.Load() }
func (s *Sim) Checkpoints() uint64 { return s.checkpointN.Load() }

func (s *Sim) LastError() string {
	if v := s.lastErr.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// Read runs fn with a read lock held. The callback style keeps the lock scope
// obvious at every call site, which matters because a handler that forgets to
// unlock stalls the simulation loop rather than just itself.
func (s *Sim) Read(fn func(e *engine.Engine)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(s.E)
}

// Write runs fn with the write lock held.
func (s *Sim) Write(fn func(e *engine.Engine)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.E)
}

// ---------------------------------------------------------------- manager --

// Options configure a Manager.
type Options struct {
	Store store.Store
	// CheckpointEvery in ticks. 0 disables automatic checkpointing.
	CheckpointEvery units.Tick
	// KeepCheckpoints bounds retention per simulation.
	KeepCheckpoints int
	// MaxSims caps concurrently registered simulations.
	MaxSims int
}

func DefaultOptions() Options {
	return Options{
		Store:           store.NewMemory(),
		CheckpointEvery: 30 * units.TicksPerMinute,
		KeepCheckpoints: 12,
		MaxSims:         64,
	}
}

type Manager struct {
	opt Options

	mu    sync.RWMutex
	sims  map[string]*Sim
	order []string
	seq   atomic.Uint64

	// maps caches generated worlds so that a parent and all of its forks share
	// exactly one copy by pointer. This is the memory story behind
	// counterfactuals: 20 scenarios on the "large" preset share one ~40 MB
	// world and pay only for their own dynamic state.
	mapMu sync.Mutex
	maps  map[string]*world.Map
}

func NewManager(opt Options) *Manager {
	if opt.Store == nil {
		opt.Store = store.NewMemory()
	}
	if opt.MaxSims <= 0 {
		opt.MaxSims = 64
	}
	return &Manager{opt: opt, sims: make(map[string]*Sim), maps: make(map[string]*world.Map)}
}

func (m *Manager) mapFor(cfg engine.Config) *world.Map {
	key := fmt.Sprintf("%s/%d", cfg.Preset, cfg.Seed)
	m.mapMu.Lock()
	defer m.mapMu.Unlock()
	if w, ok := m.maps[key]; ok {
		return w
	}
	w := world.Generate(world.DefaultParams(cfg.Preset, cfg.Seed))
	m.maps[key] = w
	return w
}

func (m *Manager) nextID(prefix string) string {
	return fmt.Sprintf("%s-%04d", prefix, m.seq.Add(1))
}

// Create starts a new root simulation.
func (m *Manager) Create(name string, cfg engine.Config) (*Sim, error) {
	m.mu.Lock()
	if len(m.sims) >= m.opt.MaxSims {
		m.mu.Unlock()
		return nil, fmt.Errorf("simctl: simulation limit of %d reached", m.opt.MaxSims)
	}
	m.mu.Unlock()

	e := engine.NewWithMap(m.mapFor(cfg), cfg)
	if name == "" {
		name = "Baseline"
	}
	s := &Sim{
		ID: m.nextID("sim"), Name: name, Created: time.Now().UTC(), Cfg: cfg, E: e,
		wake: make(chan struct{}, 1), closed: make(chan struct{}),
	}
	s.speed.Store(8)
	m.register(s)
	go m.run(s)
	return s, nil
}

// Fork branches a scenario from a running simulation.
//
// The fork copies the parent's dynamic state and command log and shares its
// map by pointer. It does NOT re-simulate from tick 0: the whole value of a
// counterfactual is that both arms start from a state that is *identical by
// construction* rather than identical by luck, and cloning is the only way to
// guarantee that when the parent has been running for simulated hours.
func (m *Manager) Fork(parentID, name string) (*Sim, error) {
	m.mu.RLock()
	parent, ok := m.sims[parentID]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("simctl: no simulation %q", parentID)
	}
	m.mu.RLock()
	n := len(m.sims)
	m.mu.RUnlock()
	if n >= m.opt.MaxSims {
		return nil, fmt.Errorf("simctl: simulation limit of %d reached", m.opt.MaxSims)
	}

	var child *Sim
	var err error
	parent.mu.RLock()
	branch := parent.E.S.Tick
	clone := parent.E.S.Clone()
	log := parent.E.Log.Clone()
	sharedMap := parent.E.Map
	cfg := parent.Cfg
	parent.mu.RUnlock()

	ce, err := engine.NewFromState(sharedMap, clone, log, cfg)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = fmt.Sprintf("Fork of %s at %d", parent.Name, branch)
	}
	child = &Sim{
		ID: m.nextID("scn"), Name: name, ParentID: parentID, BranchTick: branch,
		Created: time.Now().UTC(), Cfg: cfg, E: ce,
		wake: make(chan struct{}, 1), closed: make(chan struct{}),
	}
	child.speed.Store(parent.Speed())
	m.register(child)
	go m.run(child)
	return child, nil
}

func (m *Manager) register(s *Sim) {
	m.mu.Lock()
	m.sims[s.ID] = s
	m.order = append(m.order, s.ID)
	m.mu.Unlock()
}

func (m *Manager) Get(id string) (*Sim, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sims[id]
	return s, ok
}

func (m *Manager) List() []*Sim {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Sim, 0, len(m.sims))
	for _, id := range m.order {
		if s, ok := m.sims[id]; ok {
			out = append(out, s)
		}
	}
	return out
}

// Delete stops and removes a simulation.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	s, ok := m.sims[id]
	if ok {
		delete(m.sims, id)
		for i, o := range m.order {
			if o == id {
				m.order = append(m.order[:i], m.order[i+1:]...)
				break
			}
		}
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("simctl: no simulation %q", id)
	}
	s.stop()
	return nil
}

func (s *Sim) stop() { s.once.Do(func() { close(s.closed) }) }

func (s *Sim) nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Play, Pause and SetSpeed are the transport controls.
func (m *Manager) Play(id string) error  { return m.setRun(id, Running) }
func (m *Manager) Pause(id string) error { return m.setRun(id, Paused) }

func (m *Manager) setRun(id string, st RunState) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("simctl: no simulation %q", id)
	}
	if s.State() == Completed || s.State() == Failed {
		return errors.New("simctl: simulation has finished")
	}
	s.runState.Store(int32(st))
	s.nudge()
	return nil
}

func (m *Manager) SetSpeed(id string, speed int32) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("simctl: no simulation %q", id)
	}
	if speed > 5000 {
		speed = 5000
	}
	s.speed.Store(speed)
	s.nudge()
	return nil
}

// RunUntil schedules the simulation to stop at a tick. Used by the AI agent's
// simulate_scenario tool, which needs a bounded run rather than an open-ended
// one.
func (m *Manager) RunUntil(id string, tick units.Tick) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("simctl: no simulation %q", id)
	}
	s.stopAt.Store(uint64(tick))
	s.runState.Store(int32(Running))
	s.nudge()
	return nil
}

// Inject appends a command and mirrors it to the durable log.
func (m *Manager) Inject(id string, ev events.Event) (events.Event, error) {
	s, ok := m.Get(id)
	if !ok {
		return ev, fmt.Errorf("simctl: no simulation %q", id)
	}
	var out events.Event
	var err error
	s.Write(func(e *engine.Engine) {
		if ev.Tick == 0 {
			ev.Tick = e.S.Tick
		}
		out, err = e.Inject(ev)
	})
	if err != nil {
		return out, err
	}
	// Persisting the command is what makes the run recoverable. It is done
	// outside the engine lock: the store may be slow, and a slow disk must
	// never stall the simulation loop.
	if perr := m.opt.Store.AppendCommands(s.ID, []events.Event{out}); perr != nil {
		s.lastErr.Store("command persist failed: " + perr.Error())
	}
	return out, nil
}

// ---------------------------------------------------------------- runner ---

// run is the per-simulation driver goroutine.
//
// Pacing is done by comparing elapsed wall time against the simulated time that
// should have passed, and running whole ticks to catch up -- not by sleeping a
// fixed interval per tick. A fixed sleep would make the achievable rate a
// function of the OS timer resolution (about 1ms on Windows, so a hard ceiling
// of 1000 ticks/sec regardless of CPU); batching ticks between sleeps removes
// that ceiling entirely.
func (m *Manager) run(s *Sim) {
	const slice = 4 * time.Millisecond
	var (
		last    = time.Now()
		carry   float64
		tpsAcc  int64
		tpsMark = time.Now()
	)
	for {
		select {
		case <-s.closed:
			return
		default:
		}

		if s.State() != Running {
			select {
			case <-s.closed:
				return
			case <-s.wake:
			case <-time.After(50 * time.Millisecond):
			}
			last = time.Now()
			carry = 0
			continue
		}

		speed := s.Speed()
		now := time.Now()
		elapsed := now.Sub(last)
		last = now

		var budget int
		if speed <= 0 {
			budget = 2000 // unbounded mode: a big slice, then yield
		} else {
			carry += elapsed.Seconds() * float64(speed) * units.TicksPerSecond
			if carry > 20000 {
				// Clamp catch-up. If the process was descheduled for a second,
				// replaying a full second of simulated time in one burst would
				// produce a visible stall and a useless spike in every latency
				// metric. Dropping the backlog is the right call: this is a
				// real-time visualisation, not a video encoder.
				carry = 20000
			}
			budget = int(carry)
			carry -= float64(budget)
		}
		if budget <= 0 {
			time.Sleep(slice)
			continue
		}

		stopAt := units.Tick(s.stopAt.Load())
		deadline := time.Now().Add(slice)
		ran := 0
		s.mu.Lock()
		for ran < budget {
			if stopAt != 0 && s.E.S.Tick >= stopAt {
				break
			}
			s.E.Tick()
			ran++
			if ran%64 == 0 && time.Now().After(deadline) {
				break
			}
		}
		tick := s.E.S.Tick
		needCkpt := m.opt.CheckpointEvery > 0 &&
			tick/m.opt.CheckpointEvery != (tick-units.Tick(ran))/m.opt.CheckpointEvery
		var blob []byte
		var hdr store.Header
		if needCkpt && ran > 0 {
			blob = s.E.S.Encode()
			hdr = store.Header{Tick: tick, MapHash: s.E.Map.Hash, Digest: s.E.S.Digest()}
		}
		s.mu.Unlock()

		s.ticksRun.Add(uint64(ran))
		tpsAcc += int64(ran)
		if d := time.Since(tpsMark); d > 500*time.Millisecond {
			s.lastTPS.Store(tpsAcc * int64(time.Second) / int64(d))
			tpsAcc, tpsMark = 0, time.Now()
		}

		if blob != nil {
			t0 := time.Now()
			if _, err := m.opt.Store.PutCheckpoint(s.ID, hdr, blob); err != nil {
				s.lastErr.Store("checkpoint failed: " + err.Error())
			} else {
				s.checkpointN.Add(1)
				s.Write(func(e *engine.Engine) {
					e.Ring.Push(events.E(tick, events.EvtCheckpointWritten, events.SevInfo, -1,
						int64(tick), int64(len(blob)), time.Since(t0).Milliseconds(), 0))
				})
				m.pruneCheckpoints(s.ID)
			}
		}

		if stopAt != 0 && tick >= stopAt {
			s.runState.Store(int32(Paused))
			s.stopAt.Store(0)
		}
		if ran == 0 {
			time.Sleep(slice)
		}
	}
}

func (m *Manager) pruneCheckpoints(id string) {
	if m.opt.KeepCheckpoints <= 0 {
		return
	}
	hs, err := m.opt.Store.ListCheckpoints(id)
	if err != nil || len(hs) <= m.opt.KeepCheckpoints {
		return
	}
	// Retention is not implemented as a delete on the Store interface on
	// purpose: the filesystem and Postgres backends want very different
	// strategies (unlink vs. a partition drop), and neither is on the critical
	// path. The overflow is reported so it is visible rather than silent.
	s, ok := m.Get(id)
	if ok {
		s.lastErr.Store(fmt.Sprintf("checkpoint retention: %d kept, limit %d", len(hs), m.opt.KeepCheckpoints))
	}
}

// Restore rolls a simulation back to a stored checkpoint and replays the
// commands recorded after it.
//
// This is both the crash-recovery path and the user-facing "rewind" button.
// They are the same code path on purpose: a recovery mechanism that is only
// exercised during an incident is a recovery mechanism that does not work.
func (m *Manager) Restore(id string, tick units.Tick) (units.Tick, int, error) {
	s, ok := m.Get(id)
	if !ok {
		return 0, 0, fmt.Errorf("simctl: no simulation %q", id)
	}
	var hdr store.Header
	var raw []byte
	var err error
	if tick == 0 {
		hdr, raw, err = m.opt.Store.LatestCheckpoint(id)
	} else {
		hdr, raw, err = m.opt.Store.GetCheckpoint(id, tick)
	}
	if err != nil {
		return 0, 0, err
	}
	st, err := state.Decode(raw)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %v", store.ErrCorrupt, err)
	}
	if st.Digest() != hdr.Digest {
		return 0, 0, fmt.Errorf("%w: digest %016x, header says %016x", store.ErrCorrupt, st.Digest(), hdr.Digest)
	}

	prevState := s.State()
	s.runState.Store(int32(Paused))
	defer s.runState.Store(int32(prevState))

	var replayed int
	s.mu.Lock()
	defer s.mu.Unlock()
	target := s.E.S.Tick
	log := s.E.Log.Clone()
	ne, err := engine.NewFromState(s.E.Map, st, log, s.Cfg)
	if err != nil {
		return 0, 0, err
	}
	for ne.S.Tick < target {
		ne.Tick()
		replayed++
	}
	ne.Ring.Push(events.E(ne.S.Tick, events.EvtWorkerRecovered, events.SevNotice, -1,
		int64(0), int64(hdr.Tick), int64(replayed), 0))
	s.E = ne
	return hdr.Tick, replayed, nil
}

// StoreRef exposes the backing store for the API layer.
func (m *Manager) StoreRef() store.Store { return m.opt.Store }

// Shutdown stops every runner.
func (m *Manager) Shutdown() {
	for _, s := range m.List() {
		s.stop()
	}
}
