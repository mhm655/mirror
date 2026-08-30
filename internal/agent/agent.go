package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/mirror-sim/mirror/internal/simctl"
)

// Request is one turn from an operator.
type Request struct {
	SimID   string
	Message string
	// AllowMutations grants access to the TierMutate tools for this request
	// only. It is not sticky and is not stored: every turn that wants to
	// change a live simulation must ask again.
	AllowMutations bool
	Actor          string
}

// Step records one tool invocation, for the transcript the UI shows.
//
// Every tool call is surfaced. An agent that reports a conclusion without
// showing which tools produced it is indistinguishable from one that made the
// conclusion up, and the entire point of this layer is that it does not.
type Step struct {
	Tool   string          `json:"tool"`
	Tier   string          `json:"tier"`
	Input  json.RawMessage `json:"input"`
	Output json.RawMessage `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
	Millis int64           `json:"millis"`
}

type Response struct {
	Reply string `json:"reply"`
	Steps []Step `json:"steps"`
	Model string `json:"model"`
	// Planner is "llm" when a model drove the loop and "builtin" when the
	// deterministic planner did. Reported so nobody mistakes one for the other.
	Planner   string `json:"planner"`
	TurnCount int    `json:"turns"`
	Truncated bool   `json:"truncated,omitempty"`
	Notice    string `json:"notice,omitempty"`
}

// Agent is the operations assistant.
type Agent struct {
	mgr   *simctl.Manager
	llm   *anthropicClient
	model string
}

func New(mgr *simctl.Manager) *Agent {
	a := &Agent{mgr: mgr, model: envOr("MIRROR_LLM_MODEL", "claude-opus-5")}
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		a.llm = newAnthropic(key, a.model)
	}
	return a
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// HasModel reports whether an LLM is configured.
func (a *Agent) HasModel() bool { return a.llm != nil }

const systemPrompt = `You are the operations assistant for MIRROR, a real-time digital twin of a city.

You have tools that read the live simulation and tools that run counterfactual scenarios. Use them. Never state a number you have not read from a tool, and never speculate about what an intervention would do when you could fork a scenario and measure it.

How to answer a "what if" question:
1. Call simulate_scenario once per option you are considering, including a do-nothing baseline, all for the SAME number of simulated minutes.
2. Call compare_scenarios on the resulting ids, baseline first.
3. Report what changed, by how much, and say plainly when a difference is too small to be meaningful.

Rules:
- Prefer simulate_scenario over inject_event and set_policy. Those change the live simulation the operator is watching.
- If a mutating tool is unavailable, say that the operator has not granted mutation authority for this turn. Do not attempt a workaround.
- Percentiles computed over fewer than a few hundred completed trips are noisy. Say so rather than reporting them as fact.
- Be concise and concrete. An operator wants the number and the decision, not an essay.
- Units: travel and response times are in seconds, distances in km, fuel in litres.`

// Chat runs one turn.
func (a *Agent) Chat(ctx context.Context, req Request) (*Response, error) {
	tools := a.available(req)
	if a.llm == nil {
		return a.builtinPlan(ctx, req, tools)
	}
	resp, err := a.llmLoop(ctx, req, tools)
	if err != nil {
		// A model outage must not take the feature down. Falling back to the
		// deterministic planner keeps the platform usable and -- because the
		// planner calls the same tools -- keeps the answer grounded in real
		// simulation data rather than in an apology.
		slog.Warn("agent: model call failed, falling back to the builtin planner", "err", err)
		fb, ferr := a.builtinPlan(ctx, req, tools)
		if ferr != nil {
			return nil, err
		}
		fb.Notice = "The language model was unavailable, so this answer was produced by the built-in planner: " + err.Error()
		return fb, nil
	}
	return resp, nil
}

func (a *Agent) callTool(t toolImpl, req Request, args map[string]any) Step {
	start := time.Now()
	raw, _ := json.Marshal(args)
	step := Step{Tool: t.spec.Name, Tier: t.tier.String(), Input: raw}
	out, err := t.run(a, req, args)
	step.Millis = time.Since(start).Milliseconds()
	if err != nil {
		step.Error = err.Error()
		return step
	}
	b, mErr := json.Marshal(out)
	if mErr != nil {
		step.Error = mErr.Error()
		return step
	}
	// Tool results go into the model's context, so they are capped. A tool
	// that can return an unbounded blob is a tool that can blow the context
	// window and take the whole turn with it.
	const maxResult = 24000
	if len(b) > maxResult {
		b = append(b[:maxResult], []byte(`... (truncated)"}`)...)
	}
	step.Output = b
	return step
}

func findTool(tools []toolImpl, name string) (toolImpl, bool) {
	for _, t := range tools {
		if t.spec.Name == name {
			return t, true
		}
	}
	return toolImpl{}, false
}

// ------------------------------------------------------- builtin planner ---

// builtinPlan answers without a language model.
//
// This is not a stub. It classifies the request, runs the same tools an LLM
// would, and writes a report from the results. Two reasons it exists:
//
//  1. The platform must be fully demonstrable without an API key. A portfolio
//     project whose headline feature needs someone else's billing account is
//     not a demonstrable project.
//  2. It is the fallback when the model is unreachable, which keeps an outage
//     in a third-party API from becoming an outage in the operations console.
//
// It is genuinely worse than a model at open-ended questions, and it says so
// rather than bluffing.
func (a *Agent) builtinPlan(ctx context.Context, req Request, tools []toolImpl) (*Response, error) {
	msg := strings.ToLower(req.Message)
	resp := &Response{Planner: "builtin", Model: "none"}
	run := func(name string, args map[string]any) (Step, bool) {
		t, ok := findTool(tools, name)
		if !ok {
			return Step{}, false
		}
		st := a.callTool(t, req, args)
		resp.Steps = append(resp.Steps, st)
		return st, st.Error == ""
	}

	switch {
	case containsAny(msg, "what if", "would it help", "should we", "compare", "adaptive", "more buses", "congestion charge", "speed limit"):
		return a.builtinCounterfactual(ctx, req, tools, msg, resp)

	case containsAny(msg, "hospital", "casualt", "bed", "ambulance"):
		st, ok := run("get_hospital_capacity", map[string]any{"sim": req.SimID})
		if !ok {
			return nil, fmt.Errorf("hospital query failed")
		}
		resp.Reply = summariseHospitals(st)

	case containsAny(msg, "power", "substation", "blackout", "outage", "electric"):
		st, ok := run("get_power_status", map[string]any{"sim": req.SimID})
		if !ok {
			return nil, fmt.Errorf("power query failed")
		}
		resp.Reply = summarisePower(st)

	case containsAny(msg, "transit", "bus", "metro", "passenger"):
		st, ok := run("get_transit_load", map[string]any{"sim": req.SimID})
		if !ok {
			return nil, fmt.Errorf("transit query failed")
		}
		resp.Reply = summariseTransit(st)

	case containsAny(msg, "incident", "accident", "crash", "emergency", "response"):
		run("inspect_incident", map[string]any{"sim": req.SimID})
		st, _ := run("get_metrics", map[string]any{"sim": req.SimID})
		resp.Reply = summariseIncidents(resp.Steps, st)

	case containsAny(msg, "event", "log", "what happened", "recent"):
		st, ok := run("query_events", map[string]any{"sim": req.SimID, "minSeverity": "warning", "limit": 25})
		if !ok {
			return nil, fmt.Errorf("event query failed")
		}
		resp.Reply = summariseEvents(st)

	default:
		run("get_traffic", map[string]any{"sim": req.SimID})
		run("get_population", map[string]any{"sim": req.SimID})
		st, _ := run("get_metrics", map[string]any{"sim": req.SimID})
		resp.Reply = summariseCity(resp.Steps, st)
	}
	resp.TurnCount = 1
	if resp.Notice == "" {
		resp.Notice = "Answered by the built-in planner. Set ANTHROPIC_API_KEY to enable open-ended reasoning over the same tools."
	}
	return resp, nil
}

// builtinCounterfactual actually runs the experiment.
func (a *Agent) builtinCounterfactual(ctx context.Context, req Request, tools []toolImpl, msg string, resp *Response) (*Response, error) {
	if req.SimID == "" {
		resp.Reply = "Tell me which simulation to work from, or open one first."
		return resp, nil
	}
	minutes := int64(30)
	if strings.Contains(msg, "hour") {
		minutes = 60
	}

	// Always include a do-nothing arm. Without it, a comparison has nothing to
	// attribute the difference to.
	arms := []struct {
		name   string
		policy map[string]any
	}{
		{"A - no intervention", map[string]any{}},
	}
	switch {
	case containsAny(msg, "adaptive", "signal", "light"):
		arms = append(arms, struct {
			name   string
			policy map[string]any
		}{"B - adaptive signals", map[string]any{"adaptiveSignals": true}})
	case containsAny(msg, "bus", "transit", "metro"):
		arms = append(arms, struct {
			name   string
			policy map[string]any
		}{"B - transit fleet +60%", map[string]any{"transitVehiclesPct": int64(160)}})
	case containsAny(msg, "congestion charge", "charge", "toll"):
		arms = append(arms, struct {
			name   string
			policy map[string]any
		}{"B - congestion charge", map[string]any{"congestionCharge": true}})
	case containsAny(msg, "speed limit", "slower", "20mph", "30km"):
		arms = append(arms, struct {
			name   string
			policy map[string]any
		}{"B - speed limits at 80%", map[string]any{"speedLimitPct": int64(80)}})
	default:
		arms = append(arms,
			struct {
				name   string
				policy map[string]any
			}{"B - adaptive signals", map[string]any{"adaptiveSignals": true}},
			struct {
				name   string
				policy map[string]any
			}{"C - adaptive signals + transit +60%", map[string]any{"adaptiveSignals": true, "transitVehiclesPct": int64(160)}})
	}

	t, ok := findTool(tools, "simulate_scenario")
	if !ok {
		return nil, fmt.Errorf("simulate_scenario is unavailable")
	}
	var ids []string
	for _, arm := range arms {
		args := map[string]any{"sim": req.SimID, "name": arm.name, "minutes": minutes}
		for k, v := range arm.policy {
			args[k] = v
		}
		st := a.callTool(t, req, args)
		resp.Steps = append(resp.Steps, st)
		if st.Error != "" {
			continue
		}
		var payload struct {
			ScenarioID string `json:"scenarioId"`
		}
		if err := json.Unmarshal(st.Output, &payload); err == nil && payload.ScenarioID != "" {
			ids = append(ids, payload.ScenarioID)
		}
		if ctx.Err() != nil {
			resp.Truncated = true
			break
		}
	}
	if len(ids) < 2 {
		resp.Reply = "I could not complete enough scenario runs to make a comparison."
		return resp, nil
	}
	ct, _ := findTool(tools, "compare_scenarios")
	anyIDs := make([]any, len(ids))
	for i, v := range ids {
		anyIDs[i] = v
	}
	cs := a.callTool(ct, req, map[string]any{"ids": anyIDs})
	resp.Steps = append(resp.Steps, cs)
	resp.Reply = summariseComparison(cs, minutes)
	resp.TurnCount = len(resp.Steps)
	return resp, nil
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
