package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mirror-sim/mirror/internal/simctl"
)

func TestContainsAny(t *testing.T) {
	if !containsAny("what if we added more buses", "what if", "compare") {
		t.Error("expected a match on 'what if'")
	}
	if containsAny("how many hospitals are there", "what if", "compare") {
		t.Error("expected no match")
	}
}

// newTestAgent builds an Agent with no ANTHROPIC_API_KEY, so Chat always runs
// the deterministic builtin planner -- the thing this test file exercises.
func newTestAgent(t *testing.T) (*Agent, *simctl.Manager, *simctl.Sim) {
	t.Helper()
	os.Unsetenv("ANTHROPIC_API_KEY")
	mgr := simctl.NewManager(simctl.DefaultOptions())
	t.Cleanup(mgr.Shutdown)
	sim, err := mgr.Create("test", smallCfg())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	a := New(mgr)
	if a.HasModel() {
		t.Fatal("expected no LLM to be configured for this test")
	}
	return a, mgr, sim
}

func TestChatWithoutSimIDAsksWhichSimulation(t *testing.T) {
	a, _, _ := newTestAgent(t)
	resp, err := a.Chat(context.Background(), Request{Message: "what if we added adaptive signals"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Planner != "builtin" {
		t.Errorf("Planner = %q, want builtin", resp.Planner)
	}
	if resp.Reply == "" {
		t.Fatal("expected a reply asking which simulation to use")
	}
}

func TestChatTrafficQuestionCallsExpectedTools(t *testing.T) {
	a, _, sim := newTestAgent(t)
	resp, err := a.Chat(context.Background(), Request{SimID: sim.ID, Message: "how is the city doing right now"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Planner != "builtin" {
		t.Errorf("Planner = %q, want builtin", resp.Planner)
	}
	if len(resp.Steps) == 0 {
		t.Fatal("expected at least one tool call step")
	}
	sawTraffic := false
	for _, s := range resp.Steps {
		if s.Tool == "get_traffic" {
			sawTraffic = true
		}
		if s.Error != "" {
			t.Errorf("tool %q returned an error: %s", s.Tool, s.Error)
		}
	}
	if !sawTraffic {
		t.Errorf("expected get_traffic to be called for a general city question, steps: %+v", resp.Steps)
	}
}

func TestChatHospitalQuestionRoutesToHospitalTool(t *testing.T) {
	a, _, sim := newTestAgent(t)
	resp, err := a.Chat(context.Background(), Request{SimID: sim.ID, Message: "what is hospital capacity like"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.Steps) != 1 || resp.Steps[0].Tool != "get_hospital_capacity" {
		t.Fatalf("expected exactly one get_hospital_capacity step, got %+v", resp.Steps)
	}
}

func TestChatDoesNotUseMutatingToolsWithoutApproval(t *testing.T) {
	a, _, sim := newTestAgent(t)
	resp, err := a.Chat(context.Background(), Request{SimID: sim.ID, Message: "how is the city doing", AllowMutations: false})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	for _, s := range resp.Steps {
		if s.Tier == "mutate" {
			t.Errorf("a mutate-tier tool (%s) was used without AllowMutations", s.Tool)
		}
	}
}

func TestChatCounterfactualForksAndCompares(t *testing.T) {
	a, mgr, sim := newTestAgent(t)
	before := len(mgr.List())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := a.Chat(ctx, Request{SimID: sim.ID, Message: "what if we turned on adaptive signals"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Reply == "" {
		t.Fatal("expected a comparison report")
	}

	sawSimulate, sawCompare := false, false
	for _, s := range resp.Steps {
		switch s.Tool {
		case "simulate_scenario":
			sawSimulate = true
		case "compare_scenarios":
			sawCompare = true
		}
		if s.Error != "" {
			t.Errorf("tool %q errored: %s", s.Tool, s.Error)
		}
	}
	if !sawSimulate {
		t.Error("expected simulate_scenario to be called for a what-if question")
	}
	if !sawCompare {
		t.Error("expected compare_scenarios to be called once the arms had run")
	}

	after := len(mgr.List())
	if after <= before {
		t.Errorf("expected forked scenarios to be registered on the manager: before=%d after=%d", before, after)
	}
}
