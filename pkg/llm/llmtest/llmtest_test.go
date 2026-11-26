package llmtest

import (
	"errors"
	"testing"

	"github.com/divmora/zenith/pkg/llm"
)

func TestScript(t *testing.T) {
	c := New(
		Call("c1", "ls", map[string]string{"path": "."}),
		Step{Reply: func(req *llm.Request) (*llm.Response, error) {
			last := req.Messages[len(req.Messages)-1]
			if len(last.ToolResults) != 1 || last.ToolResults[0].CallID != "c1" {
				return nil, errors.New("tool result missing")
			}
			return &llm.Response{Message: llm.Message{Text: "done"}}, nil
		}},
	)
	var events []llm.Event
	req := &llm.Request{Messages: []llm.Message{{Role: llm.User, Text: "go"}}}
	r1, err := c.Chat(t.Context(), req, func(e llm.Event) { events = append(events, e) })
	if err != nil || r1.StopReason != llm.StopToolUse || string(r1.Message.ToolCalls[0].Input) != `{"path":"."}` {
		t.Fatalf("step 1: %+v %v", r1, err)
	}
	if len(events) != 1 || events[0].Type != llm.EventToolStart {
		t.Errorf("events = %+v", events)
	}
	req.Messages = append(req.Messages, r1.Message, llm.Message{Role: llm.User, ToolResults: []llm.ToolResult{{CallID: "c1", Name: "ls", Content: "x"}}})
	r2, err := c.Chat(t.Context(), req, nil)
	if err != nil || r2.Message.Text != "done" || r2.StopReason != llm.StopEnd {
		t.Fatalf("step 2: %+v %v", r2, err)
	}
	if c.Remaining() != 0 || len(c.Requests()) != 2 || len(c.Requests()[0].Messages) != 1 {
		t.Errorf("recorded requests wrong: %d", len(c.Requests()))
	}
	if _, err := c.Chat(t.Context(), req, nil); err == nil {
		t.Error("call past the script should fail")
	}
}
