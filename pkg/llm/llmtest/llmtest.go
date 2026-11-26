// Package llmtest provides a scripted llm.Client for deterministic tests of
// code that drives a model, such as an agent loop.
package llmtest

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/divmora/zenith/pkg/llm"
)

// Step is one scripted model reply. Reply returns the response for the
// request it is given (so a test can assert on the request); Response and
// Err are used when Reply is nil.
type Step struct {
	Reply    func(req *llm.Request) (*llm.Response, error)
	Response *llm.Response
	Err      error
}

// Client replays Steps in order, one per Chat call, and records every
// request. It is safe for concurrent use.
type Client struct {
	ModelName string

	mu       sync.Mutex
	steps    []Step
	requests []llm.Request
}

// New returns a client that replays steps.
func New(steps ...Step) *Client {
	return &Client{ModelName: "fake:model", steps: steps}
}

// Name implements llm.Client.
func (c *Client) Name() string { return c.ModelName }

// Chat implements llm.Client. When handler is set, the reply's reasoning,
// tool starts and text are delivered as events before Chat returns.
func (c *Client) Chat(ctx context.Context, req *llm.Request, handler llm.EventHandler) (*llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	n := len(c.requests)
	c.requests = append(c.requests, cloneRequest(req))
	if n >= len(c.steps) {
		c.mu.Unlock()
		return nil, fmt.Errorf("llmtest: unexpected call %d (script has %d steps)", n+1, len(c.steps))
	}
	step := c.steps[n]
	c.mu.Unlock()

	resp, err := step.Response, step.Err
	if step.Reply != nil {
		resp, err = step.Reply(req)
	}
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("llmtest: step %d has no response", n+1)
	}
	out := *resp
	out.Message.Role = llm.Assistant
	if out.StopReason == "" {
		out.StopReason = llm.StopEnd
		if len(out.Message.ToolCalls) > 0 {
			out.StopReason = llm.StopToolUse
		}
	}
	if out.Model == "" {
		out.Model = c.ModelName
	}
	if handler != nil {
		for _, r := range out.Message.Reasoning {
			if r.Text != "" {
				handler(llm.Event{Type: llm.EventReasoning, Text: r.Text})
			}
		}
		if out.Message.Text != "" {
			handler(llm.Event{Type: llm.EventText, Text: out.Message.Text})
		}
		for _, tc := range out.Message.ToolCalls {
			handler(llm.Event{Type: llm.EventToolStart, ToolCallID: tc.ID, ToolName: tc.Name})
		}
	}
	return &out, nil
}

// Requests returns copies of the requests received so far.
func (c *Client) Requests() []llm.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]llm.Request(nil), c.requests...)
}

// Remaining reports how many scripted steps were not used.
func (c *Client) Remaining() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.steps) - len(c.requests)
}

// Text returns a step that replies with plain text.
func Text(text string) Step {
	return Step{Response: &llm.Response{Message: llm.Message{Text: text}}}
}

// Call returns a step that replies with one tool call. Input is encoded
// as JSON.
func Call(id, name string, input any) Step {
	b, err := json.Marshal(input)
	if err != nil {
		panic(fmt.Sprintf("llmtest.Call: %v", err))
	}
	return Step{Response: &llm.Response{Message: llm.Message{
		ToolCalls: []llm.ToolCall{{ID: id, Name: name, Input: b}},
	}}}
}

func cloneRequest(r *llm.Request) llm.Request {
	out := *r
	out.Messages = append([]llm.Message(nil), r.Messages...)
	out.Tools = append([]llm.Tool(nil), r.Tools...)
	return out
}
