package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// A direct Anthropic Messages client.
//
// No SDK, because this uses exactly one endpoint with one feature (tool use)
// and the request shape is a dozen lines of JSON. A dependency here would be
// larger than the code it replaces and would put a third party in the release
// path of a portfolio project. The one thing worth being careful about --
// retry behaviour on 429 and 5xx -- is implemented explicitly below rather
// than inherited.
type anthropicClient struct {
	key    string
	model  string
	http   *http.Client
	apiURL string
}

func newAnthropic(key, model string) *anthropicClient {
	return &anthropicClient{
		key: key, model: model,
		apiURL: "https://api.anthropic.com/v1/messages",
		// A generous timeout: a turn can legitimately involve several tool
		// round trips on the model side. The caller's context still bounds
		// the whole turn.
		http: &http.Client{Timeout: 90 * time.Second},
	}
}

type msgContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type message struct {
	Role    string       `json:"role"`
	Content []msgContent `json:"content"`
}

type apiTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type apiRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []message `json:"messages"`
	Tools     []apiTool `json:"tools,omitempty"`
}

type apiResponse struct {
	ID         string       `json:"id"`
	Model      string       `json:"model"`
	Role       string       `json:"role"`
	Content    []msgContent `json:"content"`
	StopReason string       `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *anthropicClient) send(ctx context.Context, req apiRequest) (*apiResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	// Retry only on the conditions that are actually transient. Retrying a
	// 400 just burns the user's context window twice.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			}
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("content-type", "application/json")
		httpReq.Header.Set("x-api-key", c.key)
		httpReq.Header.Set("anthropic-version", "2023-06-01")

		resp, err := c.http.Do(httpReq)
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("anthropic: http %d: %s", resp.StatusCode, truncate(string(raw), 300))
			continue
		}
		var out apiResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("anthropic: malformed response: %w", err)
		}
		if out.Error != nil {
			return nil, fmt.Errorf("anthropic: %s: %s", out.Error.Type, out.Error.Message)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("anthropic: http %d: %s", resp.StatusCode, truncate(string(raw), 300))
		}
		return &out, nil
	}
	return nil, lastErr
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// maxTurns bounds the tool-use loop.
//
// A loop with no bound is a loop that can spend an operator's money forever on
// a model that has decided to call get_traffic sixty times. Eight turns is
// enough for the intended workflow -- inspect, fork three scenarios, compare,
// answer -- with headroom.
const maxTurns = 8

func (a *Agent) llmLoop(ctx context.Context, req Request, tools []toolImpl) (*Response, error) {
	apiTools := make([]apiTool, 0, len(tools))
	for _, t := range tools {
		apiTools = append(apiTools, apiTool{
			Name: t.spec.Name, Description: t.spec.Description, InputSchema: t.spec.InputSchema,
		})
	}

	sys := systemPrompt
	if req.SimID != "" {
		sys += "\n\nThe operator is currently viewing simulation " + req.SimID +
			". Use that id when a tool needs one and the operator has not named a different scenario."
	}
	if !req.AllowMutations {
		sys += "\n\nThis turn has NOT been granted mutation authority: inject_event and set_policy are unavailable. " +
			"You can still fork and run scenarios, which is usually the better answer anyway."
	}

	msgs := []message{{Role: "user", Content: []msgContent{{Type: "text", Text: req.Message}}}}
	resp := &Response{Planner: "llm", Model: a.model}

	for turn := 0; turn < maxTurns; turn++ {
		out, err := a.llm.send(ctx, apiRequest{
			Model: a.model, MaxTokens: 4096, System: sys, Messages: msgs, Tools: apiTools,
		})
		if err != nil {
			return nil, err
		}
		resp.TurnCount = turn + 1

		var assistant []msgContent
		var toolUses []msgContent
		var text bytes.Buffer
		for _, c := range out.Content {
			assistant = append(assistant, c)
			switch c.Type {
			case "text":
				text.WriteString(c.Text)
			case "tool_use":
				toolUses = append(toolUses, c)
			}
		}
		msgs = append(msgs, message{Role: "assistant", Content: assistant})

		if len(toolUses) == 0 {
			resp.Reply = text.String()
			return resp, nil
		}

		results := make([]msgContent, 0, len(toolUses))
		for _, tu := range toolUses {
			t, ok := findTool(tools, tu.Name)
			if !ok {
				// The model asked for something it was not offered, which in
				// practice means it asked for a mutating tool it does not have.
				// Telling it plainly is better than a generic error: it can
				// then explain the restriction to the operator.
				results = append(results, msgContent{
					Type: "tool_result", ToolUseID: tu.ID, IsError: true,
					Content: fmt.Sprintf("The tool %q is not available for this request. "+
						"Mutating tools require the operator to grant mutation authority.", tu.Name),
				})
				continue
			}
			var args map[string]any
			if len(tu.Input) > 0 {
				_ = json.Unmarshal(tu.Input, &args)
			}
			if args == nil {
				args = map[string]any{}
			}
			step := a.callTool(t, req, args)
			resp.Steps = append(resp.Steps, step)
			if step.Error != "" {
				results = append(results, msgContent{
					Type: "tool_result", ToolUseID: tu.ID, IsError: true, Content: step.Error,
				})
				continue
			}
			results = append(results, msgContent{
				Type: "tool_result", ToolUseID: tu.ID, Content: string(step.Output),
			})
		}
		msgs = append(msgs, message{Role: "user", Content: results})

		if ctx.Err() != nil {
			resp.Truncated = true
			resp.Reply = text.String()
			if resp.Reply == "" {
				resp.Reply = "The turn ran out of time. The tool results above are complete; ask again to have them interpreted."
			}
			return resp, nil
		}
	}

	resp.Truncated = true
	resp.Reply = "I reached the tool-call limit for one turn without arriving at an answer. " +
		"The tool results so far are shown above; try asking a narrower question."
	return resp, nil
}
