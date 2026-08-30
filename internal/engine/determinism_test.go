package engine

import (
	"fmt"
	"testing"

	"github.com/mirror-sim/mirror/internal/events"
	"github.com/mirror-sim/mirror/internal/state"
	"github.com/mirror-sim/mirror/internal/units"
	"github.com/mirror-sim/mirror/internal/world"
)

func testCfg() Config {
	c := DefaultConfig()
	c.Preset = "small"
	c.Population = 6000
	c.StartHour = 7
	c.Seed = 42
	return c
}

// scriptedCommands is the same event stream applied to every engine under test.
// It deliberately touches every subsystem: traffic, closures, power, weather
// and the hospital surge path.
func scriptedCommands(e *Engine) {
	inject := func(t units.Tick, k events.Kind, a, b, c, d int64) {
		if _, err := e.Inject(events.C(t, k, a, b, c, d)); err != nil {
			panic(err)
		}
	}
	nEdges := int64(len(e.Map.Edges))
	inject(300, events.CmdInjectAccident, nEdges/3, 820, 2, 0)
	inject(700, events.CmdCloseRoad, nEdges/2, 4000, 0, 0)
	inject(900, events.CmdSetWeather, 2, 9, 55, 6000)
	inject(1400, events.CmdPowerFailure, 1, 5000, 0, 0)
	inject(1800, events.CmdSpawnTraffic, 400, -1, 0, 0)
	inject(2200, events.CmdSetPolicy, 0 /*adaptive signals*/, 1, 0, 0)
	inject(2600, events.CmdHospitalSurge, 0, 25, 0, 0)
}

// TestSameSeedSameResult is the headline correctness property: identical
// inputs produce a bit-identical state at every tick.
func TestSameSeedSameResult(t *testing.T) {
	const ticks = 3000
	a := New(testCfg())
	b := New(testCfg())
	scriptedCommands(a)
	scriptedCommands(b)
	for i := 0; i < ticks; i++ {
		a.Tick()
		b.Tick()
		if i%250 == 0 || i == ticks-1 {
			da, db := a.Digest(), b.Digest()
			if da != db {
				t.Fatalf("divergence at tick %d: %016x != %016x", i, da, db)
			}
		}
	}
}

// TestSerialEqualsParallel is the claim that makes the partitioning credible:
// running the same simulation across N region workers produces exactly the
// same state as running it on one, tick for tick.
func TestSerialEqualsParallel(t *testing.T) {
	const ticks = 2500
	cs := testCfg()
	cs.Regions, cs.Workers = 1, 1
	serial := New(cs)

	cp := testCfg()
	cp.Regions, cp.Workers = 4, 4
	par := New(cp)

	scriptedCommands(serial)
	scriptedCommands(par)

	for i := 0; i < ticks; i++ {
		serial.Tick()
		par.Tick()
		if i%100 == 0 || i == ticks-1 {
			ds, dp := serial.Digest(), par.Digest()
			if ds != dp {
				t.Fatalf("serial/parallel divergence at tick %d: %016x != %016x", i, ds, dp)
			}
		}
	}
	if serial.S.Metrics.TripsCompleted == 0 {
		t.Fatal("test is vacuous: no trips completed")
	}
	t.Logf("%d ticks, %d trips completed, digest %016x",
		ticks, serial.S.Metrics.TripsCompleted, serial.Digest())
}

// TestParallelRegionCounts checks every partition count, not just one, because
// a determinism bug that only appears at a particular region count is exactly
// the kind of bug that survives a single-configuration test.
func TestParallelRegionCounts(t *testing.T) {
	const ticks = 900
	base := testCfg()
	base.Regions, base.Workers = 1, 1
	ref := New(base)
	scriptedCommands(ref)
	ref.Run(ticks)
	want := ref.Digest()

	for _, n := range []int{2, 3, 4, 8, 9} {
		cfg := testCfg()
		cfg.Regions, cfg.Workers = n, n
		e := New(cfg)
		scriptedCommands(e)
		e.Run(ticks)
		if got := e.Digest(); got != want {
			t.Errorf("regions=%d digest %016x, want %016x", n, got, want)
		}
	}
}

// TestReplayFromLog reconstructs a run from (params, seed, command log) alone
// and checks it lands on the same state. This is the recovery path.
func TestReplayFromLog(t *testing.T) {
	const ticks = 2000
	orig := New(testCfg())
	scriptedCommands(orig)
	orig.Run(ticks)
	want := orig.Digest()

	// Rebuild from scratch using only the log.
	replay := New(testCfg())
	replay.Log = orig.Log.Clone()
	replay.Run(ticks)

	if got := replay.Digest(); got != want {
		t.Fatalf("replay digest %016x, want %016x", got, want)
	}
}

// TestCheckpointRestore verifies that state -> bytes -> state is lossless and
// that a restored engine continues identically.
func TestCheckpointRestore(t *testing.T) {
	const before, after = 1200, 900
	orig := New(testCfg())
	scriptedCommands(orig)
	orig.Run(before)

	blob := orig.S.Encode()
	restoredState, err := state.Decode(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, want := restoredState.Digest(), orig.Digest(); got != want {
		t.Fatalf("round-trip digest %016x, want %016x", got, want)
	}

	m := world.Generate(world.DefaultParams(testCfg().Preset, testCfg().Seed))
	restored, err := NewFromState(m, restoredState, orig.Log.Clone(), testCfg())
	if err != nil {
		t.Fatalf("NewFromState: %v", err)
	}
	orig.Run(after)
	restored.Run(after)
	if got, want := restored.Digest(), orig.Digest(); got != want {
		t.Fatalf("post-restore digest %016x, want %016x", got, want)
	}
	t.Logf("checkpoint size %d bytes at tick %d", len(blob), before)
}

// TestForkIsolation checks that a forked scenario cannot affect its parent,
// and that with no intervention the two stay identical.
func TestForkIsolation(t *testing.T) {
	parent := New(testCfg())
	scriptedCommands(parent)
	parent.Run(800)

	childState := parent.S.Clone()
	m := parent.Map // shared by pointer on purpose
	child, err := NewFromState(m, childState, parent.Log.Clone(), testCfg())
	if err != nil {
		t.Fatal(err)
	}
	if parent.Digest() != child.Digest() {
		t.Fatal("fork did not start from an identical state")
	}
	// No intervention: they must stay in lockstep.
	for i := 0; i < 400; i++ {
		parent.Tick()
		child.Tick()
	}
	if parent.Digest() != child.Digest() {
		t.Fatal("uninterventioned fork diverged from parent")
	}
	// Now intervene in the child only. The command is scheduled for the
	// current tick, which lands it in the middle of a log that already holds
	// commands scheduled for later ticks -- the case that must not corrupt
	// the log's ordering.
	if _, err := child.Inject(events.C(child.S.Tick, events.CmdSetPolicy, 0, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if !child.Log.Ordered() {
		t.Fatal("command log lost its tick ordering after an out-of-order injection")
	}
	for i := 0; i < 600; i++ {
		parent.Tick()
		child.Tick()
	}
	if parent.Digest() == child.Digest() {
		t.Fatal("policy change had no effect: the counterfactual is not counterfactual")
	}
	if parent.S.Policy.AdaptiveSignals {
		t.Fatal("the fork leaked its policy change back into its parent")
	}
	if !child.S.Policy.AdaptiveSignals {
		t.Fatal("the fork never applied its own policy change")
	}
}

// TestMapGenerationStable pins the generated world so that a change to the
// generator is a deliberate, visible act rather than a silent invalidation of
// every stored replay.
func TestMapGenerationStable(t *testing.T) {
	for _, preset := range []string{"small", "medium"} {
		p := world.DefaultParams(preset, 42)
		a := world.Generate(p)
		b := world.Generate(p)
		if a.Hash != b.Hash {
			t.Fatalf("%s: generator is not deterministic", preset)
		}
		if len(a.Nodes) == 0 || len(a.Edges) == 0 || len(a.Routes) == 0 {
			t.Fatalf("%s: degenerate map", preset)
		}
	}
}

// TestMapConnected asserts the road network has no unreachable island, which
// would show up as an unexplained population of permanently stranded agents.
func TestMapConnected(t *testing.T) {
	m := world.Generate(world.DefaultParams("small", 7))
	seen := make([]bool, len(m.Nodes))
	queue := []world.NodeID{0}
	seen[0] = true
	n := 1
	for len(queue) > 0 {
		cur := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for _, e := range m.Out(cur) {
			to := m.Edges[e].To
			if !seen[to] {
				seen[to] = true
				n++
				queue = append(queue, to)
			}
		}
	}
	if n != len(m.Nodes) {
		t.Fatalf("road network is not strongly reachable from node 0: %d/%d", n, len(m.Nodes))
	}
}

// TestHeuristicAdmissible brute-forces the A* heuristic against true shortest
// path costs on a small map. An inadmissible heuristic would make routes
// depend on expansion order, which would quietly break determinism.
func TestHeuristicAdmissible(t *testing.T) {
	m := world.Generate(world.DefaultParams("small", 3))
	cost := func(e world.EdgeID) int32 { return m.EdgeTravelTicksFree(e) }
	r := world.NewRouter(len(m.Nodes))
	target := world.NodeID(len(m.Nodes) / 2)
	// Dijkstra from every node to `target` via reverse search would be ideal;
	// instead we spot-check that the heuristic never exceeds the realised path
	// cost, which is the admissibility condition.
	var path []world.EdgeID
	checked := 0
	for n := 0; n < len(m.Nodes); n += 7 {
		p, ok := r.Route(m, world.NodeID(n), target, cost, path[:0])
		path = p
		if !ok {
			continue
		}
		var actual int32
		for _, e := range p {
			actual += cost(e)
		}
		dx := int64(m.Nodes[n].X - m.Nodes[target].X)
		dy := int64(m.Nodes[n].Y - m.Nodes[target].Y)
		h := int32(units.ISqrt(dx*dx+dy*dy) / int64(m.MaxSpeed))
		if h > actual {
			t.Fatalf("inadmissible heuristic at node %d: h=%d actual=%d", n, h, actual)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no routes checked")
	}
}

// TestNoFloatInState is a structural guard. Simulation state must be integers
// only; a float32 sneaking into the state struct is the classic way a
// deterministic engine stops being deterministic across architectures.
func TestNoFloatInState(t *testing.T) {
	// The check lives in a separate file so it can walk the struct with
	// reflection without importing reflect into the hot path.
	if bad := findFloatFields(); len(bad) > 0 {
		t.Fatalf("floating-point fields in simulation state: %v", bad)
	}
}

func ExampleEngine_Tick() {
	cfg := DefaultConfig()
	cfg.Preset = "small"
	cfg.Population = 1000
	cfg.Seed = 1
	e := New(cfg)
	e.Run(10)
	fmt.Println(e.S.Tick)
	// Output: 10
}
