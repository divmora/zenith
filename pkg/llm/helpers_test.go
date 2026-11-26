package llm

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// lsTool is the tool every multi-turn test offers.
var lsTool = Tool{
	Name:        "ls",
	Description: "List files in a directory.",
	InputSchema: map[string]any{
		"type":       "object",
		"properties": map[string]any{"path": map[string]any{"type": "string"}},
		"required":   []string{"path"},
	},
}

// recorder is a fake provider endpoint: it records request bodies and
// answers call n with replies[n].
type recorder struct {
	t       *testing.T
	mu      sync.Mutex
	bodies  []map[string]any
	paths   []string
	headers []http.Header
	reply   func(n int, w http.ResponseWriter)
}

func (rc *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		rc.t.Errorf("request body is not JSON: %v: %s", err, data)
	}
	rc.mu.Lock()
	n := len(rc.bodies)
	rc.bodies = append(rc.bodies, body)
	rc.paths = append(rc.paths, r.URL.String())
	rc.headers = append(rc.headers, r.Header.Clone())
	rc.mu.Unlock()
	rc.reply(n, w)
}

func (rc *recorder) body(n int) map[string]any {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if n >= len(rc.bodies) {
		rc.t.Fatalf("request %d was not sent (got %d)", n, len(rc.bodies))
	}
	return rc.bodies[n]
}

// sse writes server-sent events, one data line each.
func sse(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		_, _ = io.WriteString(w, e+"\n\n")
	}
}

// conversation runs the shared two-turn tool conversation against c:
// turn 1 must return a call to ls, turn 2 sends its result and must end
// with text. It returns both responses and the events seen.
func conversation(t *testing.T, c Client, stream bool) (first, second *Response, events []Event) {
	t.Helper()
	ctx := context.Background()
	var handler EventHandler
	if stream {
		handler = func(e Event) { events = append(events, e) }
	}
	req := &Request{
		System:   "You are a test.",
		Messages: []Message{{Role: User, Text: "List the files."}},
		Tools:    []Tool{lsTool},
	}
	first, err := c.Chat(ctx, req, handler)
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if first.StopReason != StopToolUse || len(first.Message.ToolCalls) != 1 {
		t.Fatalf("turn 1: want one tool call, got stop=%s calls=%+v", first.StopReason, first.Message.ToolCalls)
	}
	call := first.Message.ToolCalls[0]
	if call.Name != "ls" || call.ID == "" {
		t.Fatalf("turn 1: bad call %+v", call)
	}
	var in struct{ Path string }
	if err := json.Unmarshal(call.Input, &in); err != nil || in.Path != "." {
		t.Fatalf("turn 1: input %s: want path \".\" (%v)", call.Input, err)
	}
	req.Messages = append(req.Messages, first.Message, Message{
		Role:        User,
		ToolResults: []ToolResult{{CallID: call.ID, Name: call.Name, Content: "a.go\nb.go"}},
	})
	second, err = c.Chat(ctx, req, handler)
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if second.StopReason != StopEnd || second.Message.Text != "Two files." {
		t.Fatalf("turn 2: want text end, got stop=%s text=%q", second.StopReason, second.Message.Text)
	}
	return first, second, events
}

// path walks a decoded JSON value: keys for objects, ints for arrays.
func path(t *testing.T, v any, keys ...any) any {
	t.Helper()
	cur := v
	for _, k := range keys {
		switch k := k.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				t.Fatalf("path %v: %T is not an object at %q", keys, cur, k)
			}
			cur = m[k]
		case int:
			a, ok := cur.([]any)
			if !ok || k >= len(a) {
				t.Fatalf("path %v: no index %d in %v", keys, k, cur)
			}
			cur = a[k]
		}
	}
	return cur
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func hasEvent(events []Event, typ EventType, text string) bool {
	for _, e := range events {
		if e.Type == typ && (text == "" || strings.Contains(e.Text, text) || e.ToolName == text) {
			return true
		}
	}
	return false
}

func newTestClient(t *testing.T, cfg ModelConfig) Client {
	t.Helper()
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = -1
	}
	c, err := New(cfg, quiet)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
