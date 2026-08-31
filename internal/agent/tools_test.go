package agent

import (
	"encoding/json"
	"testing"

	"github.com/mirror-sim/mirror/internal/engine"
	"github.com/mirror-sim/mirror/internal/simctl"
)

func TestNumArg(t *testing.T) {
	cases := []struct {
		in     map[string]any
		key    string
		want   int64
		wantOk bool
	}{
		{map[string]any{"n": float64(42)}, "n", 42, true},
		{map[string]any{"n": int64(7)}, "n", 7, true},
		{map[string]any{"n": int(3)}, "n", 3, true},
		{map[string]any{"n": json.Number("15")}, "n", 15, true},
		{map[string]any{"n": json.Number("nope")}, "n", 0, false},
		{map[string]any{"n": "not a number"}, "n", 0, false},
		{map[string]any{}, "missing", 0, false},
	}
	for _, c := range cases {
		got, ok := numArg(c.in, c.key)
		if got != c.want || ok != c.wantOk {
			t.Errorf("numArg(%v, %q) = (%d, %v), want (%d, %v)", c.in, c.key, got, ok, c.want, c.wantOk)
		}
	}
}

func TestB2i(t *testing.T) {
	if b2i(true) != 1 {
		t.Error("b2i(true) should be 1")
	}
	if b2i(false) != 0 {
		t.Error("b2i(false) should be 0")
	}
}

func TestMinInt(t *testing.T) {
	if minInt(3, 5) != 3 {
		t.Error("minInt(3, 5) should be 3")
	}
	if minInt(5, 3) != 3 {
		t.Error("minInt(5, 3) should be 3")
	}
	if minInt(4, 4) != 4 {
		t.Error("minInt(4, 4) should be 4")
	}
}

func TestPolicySummaryDefaults(t *testing.T) {
	got := policySummary(simctl.PolicyView{})
	if got == "" {
		t.Error("policySummary should describe the default policy, not return empty")
	}
}

// smallCfg mirrors the engine package's own determinism-test configuration:
// small preset, low population, fixed seed -- fast enough for a unit test.
func smallCfg() engine.Config {
	cfg := engine.DefaultConfig()
	cfg.Preset = "small"
	cfg.Population = 6000
	cfg.Seed = 42
	cfg.StartHour = 7
	return cfg
}

func TestAvailableFiltersMutateTierByRequest(t *testing.T) {
	mgr := simctl.NewManager(simctl.DefaultOptions())
	t.Cleanup(mgr.Shutdown)
	a := New(mgr)

	withoutMutate := a.available(Request{AllowMutations: false})
	for _, tool := range withoutMutate {
		if tool.tier == TierMutate {
			t.Fatalf("tool %q with tier=mutate should not be available without AllowMutations", tool.spec.Name)
		}
	}

	withMutate := a.available(Request{AllowMutations: true})
	if len(withMutate) <= len(withoutMutate) {
		t.Fatalf("expected more tools available with AllowMutations=true: got %d vs %d", len(withMutate), len(withoutMutate))
	}

	// Sandboxed tools (fork-and-run) must be available regardless of the
	// mutation grant: a fork cannot affect its parent, so this tier is safe by
	// construction rather than by permission (see docs/adr/ADR-013).
	if _, ok := findTool(withoutMutate, "simulate_scenario"); !ok {
		t.Fatal("simulate_scenario (sandboxed) should be available even without AllowMutations")
	}
	if _, ok := findTool(withoutMutate, "inject_event"); ok {
		t.Fatal("inject_event (mutate) should not be available without AllowMutations")
	}
	if _, ok := findTool(withMutate, "inject_event"); !ok {
		t.Fatal("inject_event (mutate) should be available with AllowMutations")
	}
}

func TestToolSpecsIncludeTierAndAllRegisteredTools(t *testing.T) {
	mgr := simctl.NewManager(simctl.DefaultOptions())
	t.Cleanup(mgr.Shutdown)
	a := New(mgr)

	specs := a.ToolSpecs()
	if len(specs) != len(a.registry()) {
		t.Fatalf("ToolSpecs returned %d entries, registry has %d", len(specs), len(a.registry()))
	}
	for _, s := range specs {
		if s.Tier == "" {
			t.Errorf("tool %q has no tier set in its spec", s.Name)
		}
	}
}

func TestSimForRequiresAnID(t *testing.T) {
	mgr := simctl.NewManager(simctl.DefaultOptions())
	t.Cleanup(mgr.Shutdown)
	a := New(mgr)

	if _, err := a.simFor(Request{}, map[string]any{}); err == nil {
		t.Fatal("simFor with no sim id anywhere should error")
	}
	if _, err := a.simFor(Request{}, map[string]any{"sim": "does-not-exist"}); err == nil {
		t.Fatal("simFor with an unknown id should error")
	}
}

func TestToolGetTrafficReadsALiveSimulation(t *testing.T) {
	mgr := simctl.NewManager(simctl.DefaultOptions())
	t.Cleanup(mgr.Shutdown)
	sim, err := mgr.Create("test", smallCfg())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	a := New(mgr)
	out, err := toolGetTraffic(a, Request{SimID: sim.ID}, map[string]any{})
	if err != nil {
		t.Fatalf("toolGetTraffic: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("toolGetTraffic returned %T, want map[string]any", out)
	}
	if _, ok := m["activeVehicles"]; !ok {
		t.Errorf("expected an activeVehicles field, got %+v", m)
	}
}
