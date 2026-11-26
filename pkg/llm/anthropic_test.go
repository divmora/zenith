package llm

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func anthropicServer(t *testing.T, stream bool) (*recorder, Client) {
	rc := &recorder{t: t}
	rc.reply = func(n int, w http.ResponseWriter) {
		switch {
		case n == 0 && !stream:
			_, _ = io.WriteString(w, `{"model":"claude-x","stop_reason":"tool_use","content":[
				{"type":"thinking","thinking":"need ls","signature":"sig1"},
				{"type":"text","text":"Checking."},
				{"type":"tool_use","id":"toolu_1","name":"ls","input":{"path":"."}}],
				"usage":{"input_tokens":3,"output_tokens":5,"cache_creation_input_tokens":100,"cache_read_input_tokens":7}}`)
		case n == 1 && !stream:
			_, _ = io.WriteString(w, `{"model":"claude-x","stop_reason":"end_turn","content":[{"type":"text","text":"Two files."}],"usage":{"input_tokens":20,"output_tokens":3}}`)
		case n == 0:
			sse(w,
				"event: message_start\ndata: "+`{"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":3,"cache_read_input_tokens":7,"cache_creation_input_tokens":100,"output_tokens":1}}}`,
				"event: content_block_start\ndata: "+`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
				"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"need "}}`,
				"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"ls"}}`,
				"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig1"}}`,
				"event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":0}`,
				"event: ping\ndata: "+`{"type":"ping"}`,
				"event: content_block_start\ndata: "+`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
				"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Checking."}}`,
				"event: content_block_start\ndata: "+`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"ls","input":{}}}`,
				"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
				"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\".\"}"}}`,
				"event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":2}`,
				"event: message_delta\ndata: "+`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
				"event: message_stop\ndata: "+`{"type":"message_stop"}`,
			)
		default:
			sse(w,
				"event: message_start\ndata: "+`{"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":20,"output_tokens":1}}}`,
				"event: content_block_start\ndata: "+`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Two files."}}`,
				"event: message_delta\ndata: "+`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
				"event: message_stop\ndata: "+`{"type":"message_stop"}`,
			)
		}
	}
	srv := httptest.NewServer(rc)
	t.Cleanup(srv.Close)
	return rc, newTestClient(t, ModelConfig{Provider: "anthropic", Model: "claude-x", APIKey: "k", BaseURL: srv.URL})
}

func TestAnthropicConversation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stream"}[stream], func(t *testing.T) {
			rc, c := anthropicServer(t, stream)
			first, _, events := conversation(t, c, stream)

			if r := first.Message.Reasoning; len(r) != 1 || r[0].Text != "need ls" || r[0].Signature != "sig1" {
				t.Errorf("reasoning = %+v", r)
			}
			if first.Message.Text != "Checking." {
				t.Errorf("text = %q", first.Message.Text)
			}
			want := Usage{InputTokens: 110, OutputTokens: 5, CachedInputTokens: 7, CacheWriteTokens: 100}
			if first.Usage != want {
				t.Errorf("usage = %+v, want %+v", first.Usage, want)
			}
			if stream && (!hasEvent(events, EventReasoning, "need") || !hasEvent(events, EventToolStart, "ls")) {
				t.Errorf("events = %+v", events)
			}

			b0 := rc.body(0)
			if path(t, b0, "system", 0, "text") != "You are a test." || path(t, b0, "tools", 0, "input_schema", "type") != "object" {
				t.Errorf("turn 1 body: %s", mustJSON(t, b0))
			}
			b1 := rc.body(1)
			asst := path(t, b1, "messages", 1, "content")
			if path(t, asst, 0, "type") != "thinking" || path(t, asst, 0, "signature") != "sig1" ||
				path(t, asst, 1, "text") != "Checking." ||
				path(t, asst, 2, "type") != "tool_use" || path(t, asst, 2, "input", "path") != "." {
				t.Errorf("assistant content must replay thinking, text and tool_use in order: %s", mustJSON(t, asst))
			}
			res := path(t, b1, "messages", 2, "content", 0)
			if path(t, res, "type") != "tool_result" || path(t, res, "tool_use_id") != "toolu_1" || path(t, res, "content") != "a.go\nb.go" {
				t.Errorf("tool result: %s", mustJSON(t, res))
			}
			if h := rc.headers[0]; h.Get("x-api-key") != "k" || h.Get("anthropic-version") == "" {
				t.Errorf("headers = %v", h)
			}
		})
	}
}

func TestAnthropicCacheAndChoice(t *testing.T) {
	rc := &recorder{t: t}
	rc.reply = func(_ int, w http.ResponseWriter) {
		_, _ = io.WriteString(w, `{"stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}`)
	}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	c := newTestClient(t, ModelConfig{Provider: "anthropic", Model: "m", APIKey: "k", BaseURL: srv.URL})
	_, err := c.Chat(t.Context(), &Request{
		System:     "sys",
		Messages:   []Message{{Role: User, Text: "a"}, {Role: Assistant, Text: "b"}, {Role: User, Text: "c"}},
		Tools:      []Tool{lsTool},
		ToolChoice: ToolChoice{Mode: ToolRequired},
		Cache:      true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := rc.body(0)
	for _, p := range [][]any{{"system", 0}, {"tools", 0}, {"messages", 2, "content", 0}} {
		if path(t, b, append(p, "cache_control", "type")...) != "ephemeral" {
			t.Errorf("no cache_control at %v: %s", p, mustJSON(t, b))
		}
	}
	if path(t, b, "messages", 0, "content", 0).(map[string]any)["cache_control"] != nil {
		t.Errorf("only the last message should carry cache_control")
	}
	if path(t, b, "tool_choice", "type") != "any" {
		t.Errorf("tool_choice = %v", b["tool_choice"])
	}
}

func TestAnthropicStreamErrorRetryable(t *testing.T) {
	rc := &recorder{t: t}
	rc.reply = func(n int, w http.ResponseWriter) {
		if n == 0 {
			sse(w, "event: error\ndata: "+`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			return
		}
		sse(w,
			"event: content_block_start\ndata: "+`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
			"event: message_stop\ndata: "+`{"type":"message_stop"}`,
		)
	}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	c, err := New(ModelConfig{Provider: "anthropic", Model: "m", APIKey: "k", BaseURL: srv.URL, MaxRetries: 1}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Chat(t.Context(), &Request{Messages: []Message{{Role: User, Text: "hi"}}}, func(Event) {})
	if err != nil {
		t.Fatalf("overloaded before any output should be retried: %v", err)
	}
	if resp.Message.Text != "ok" {
		t.Errorf("text = %q", resp.Message.Text)
	}
}

func TestAnthropicRedactedThinkingReplay(t *testing.T) {
	rc := &recorder{t: t}
	rc.reply = func(_ int, w http.ResponseWriter) {
		_, _ = io.WriteString(w, `{"stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}`)
	}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	c := newTestClient(t, ModelConfig{Provider: "anthropic", Model: "m", APIKey: "k", BaseURL: srv.URL})
	_, err := c.Chat(t.Context(), &Request{Messages: []Message{
		{Role: User, Text: "a"},
		{Role: Assistant, Text: "b", Reasoning: []Reasoning{{Provider: "anthropic", Redacted: "ENC"}, {Provider: "gemini", Signature: "other"}}},
		{Role: User, Text: "c"},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	asst := path(t, rc.body(0), "messages", 1, "content").([]any)
	if len(asst) != 2 || path(t, asst, 0, "type") != "redacted_thinking" || path(t, asst, 0, "data") != "ENC" {
		t.Errorf("want redacted_thinking then text, other providers' reasoning dropped: %s", mustJSON(t, asst))
	}
}
