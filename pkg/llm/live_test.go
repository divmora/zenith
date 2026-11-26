package llm

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLive runs a real two-turn tool conversation against each model in
// ZENITH_LIVE_MODELS (comma-separated provider:model specs), in both
// non-streaming and streaming mode. It is skipped unless that variable is
// set, so `make test` never needs network or credentials.
//
//	AWS_PROFILE=<profile> AWS_REGION=us-east-1 \
//	ZENITH_LIVE_MODELS=bedrock:<model-id> go test ./pkg/llm -run TestLive -v
func TestLive(t *testing.T) {
	specs := os.Getenv("ZENITH_LIVE_MODELS")
	if specs == "" {
		t.Skip("set ZENITH_LIVE_MODELS to run against real providers")
	}
	for _, spec := range strings.Split(specs, ",") {
		cfg, err := ParseSpec(spec)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Region = os.Getenv("AWS_REGION")
		cfg.MaxTokens = 1024
		cfg.Timeout = 2 * time.Minute
		for _, stream := range []bool{false, true} {
			name := spec + map[bool]string{false: "/complete", true: "/stream"}[stream]
			t.Run(name, func(t *testing.T) {
				c, err := New(cfg, quiet)
				if err != nil {
					t.Fatal(err)
				}
				liveConversation(t, c, stream)
			})
		}
	}
}

func liveConversation(t *testing.T, c Client, stream bool) {
	ctx := context.Background()
	var events []Event
	var handler EventHandler
	if stream {
		handler = func(e Event) { events = append(events, e) }
	}
	req := &Request{
		System:   "You are a coding agent. Use tools to answer. Be brief.",
		Messages: []Message{{Role: User, Text: "How many files are in the current directory \".\"? Use the ls tool."}},
		Tools:    []Tool{lsTool},
		Cache:    true,
	}
	first, err := c.Chat(ctx, req, handler)
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	t.Logf("turn 1: stop=%s raw=%s calls=%d usage=%+v reasoning=%d", first.StopReason, first.RawStop, len(first.Message.ToolCalls), first.Usage, len(first.Message.Reasoning))
	if len(first.Message.ToolCalls) == 0 {
		t.Fatalf("turn 1: model did not call ls (text %q)", first.Message.Text)
	}
	var results []ToolResult
	for _, tc := range first.Message.ToolCalls {
		var in map[string]any
		if err := json.Unmarshal(tc.Input, &in); err != nil {
			t.Fatalf("turn 1: input %s is not an object: %v", tc.Input, err)
		}
		results = append(results, ToolResult{CallID: tc.ID, Name: tc.Name, Content: "main.go\ngo.mod\nREADME.md"})
	}
	req.Messages = append(req.Messages, first.Message, Message{Role: User, ToolResults: results})
	second, err := c.Chat(ctx, req, handler)
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	t.Logf("turn 2: stop=%s text=%q usage=%+v", second.StopReason, second.Message.Text, second.Usage)
	if second.StopReason != StopEnd || !strings.Contains(second.Message.Text, "3") && !strings.Contains(strings.ToLower(second.Message.Text), "three") {
		t.Errorf("turn 2: want a final answer mentioning 3 files, got stop=%s text=%q", second.StopReason, second.Message.Text)
	}
	if stream && !hasEvent(events, EventToolStart, "ls") {
		t.Errorf("stream: no tool_start event for ls")
	}
	if first.Usage.InputTokens == 0 || first.Usage.OutputTokens == 0 {
		t.Errorf("turn 1: usage not reported: %+v", first.Usage)
	}
}
