package llm

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func openAIServer(t *testing.T, stream bool) (*recorder, Client) {
	rc := &recorder{t: t}
	rc.reply = func(n int, w http.ResponseWriter) {
		switch {
		case n == 0 && !stream:
			_, _ = io.WriteString(w, `{"model":"gpt-x","choices":[{"message":{"content":"","reasoning_content":"need ls","tool_calls":[{"id":"call_1","type":"function","function":{"name":"ls","arguments":"{\"path\":\".\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":4}}}`)
		case n == 1 && !stream:
			_, _ = io.WriteString(w, `{"model":"gpt-x","choices":[{"message":{"content":"Two files."},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":3}}`)
		case n == 0:
			sse(w,
				`data: {"model":"gpt-x","choices":[{"delta":{"reasoning_content":"need "}}]}`,
				`data: {"choices":[{"delta":{"reasoning_content":"ls"}}]}`,
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"ls","arguments":""}}]}}]}`,
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"pa"}}]}}]}`,
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\".\"}"}}]}}]}`,
				`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
				`data: [DONE]`,
			)
		default:
			sse(w,
				`data: {"choices":[{"delta":{"content":"Two "}}]}`,
				`data: {"choices":[{"delta":{"content":"files."},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			)
		}
	}
	srv := httptest.NewServer(rc)
	t.Cleanup(srv.Close)
	return rc, newTestClient(t, ModelConfig{Provider: "openai", Model: "gpt-x", APIKey: "k", BaseURL: srv.URL})
}

func TestOpenAIConversation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stream"}[stream], func(t *testing.T) {
			rc, c := openAIServer(t, stream)
			first, _, events := conversation(t, c, stream)

			if got := first.Message.Reasoning; len(got) != 1 || got[0].Text != "need ls" {
				t.Errorf("reasoning = %+v", got)
			}
			if stream {
				if !hasEvent(events, EventToolStart, "ls") || !hasEvent(events, EventText, "Two") || !hasEvent(events, EventReasoning, "need") {
					t.Errorf("missing events: %+v", events)
				}
			} else if first.Usage != (Usage{InputTokens: 10, OutputTokens: 5, CachedInputTokens: 4}) {
				t.Errorf("usage = %+v", first.Usage)
			}

			b0 := rc.body(0)
			if path(t, b0, "messages", 0, "role") != "system" || path(t, b0, "tools", 0, "function", "name") != "ls" {
				t.Errorf("turn 1 body: %s", mustJSON(t, b0))
			}
			if _, ok := b0["tool_choice"]; ok {
				t.Errorf("auto tool choice should be left out: %v", b0["tool_choice"])
			}
			if path(t, b0, "max_completion_tokens") == nil {
				t.Errorf("openai should use max_completion_tokens")
			}
			if stream && path(t, b0, "stream") != true {
				t.Errorf("stream flag missing")
			}
			b1 := rc.body(1)
			// system, user, assistant(tool_calls), tool
			asst := path(t, b1, "messages", 2)
			if path(t, asst, "role") != "assistant" || path(t, asst, "tool_calls", 0, "id") != "call_1" ||
				path(t, asst, "tool_calls", 0, "function", "arguments") != `{"path":"."}` {
				t.Errorf("assistant message: %s", mustJSON(t, asst))
			}
			if _, ok := asst.(map[string]any)["content"]; ok {
				t.Errorf("empty assistant content should be omitted: %s", mustJSON(t, asst))
			}
			tool := path(t, b1, "messages", 3)
			if path(t, tool, "role") != "tool" || path(t, tool, "tool_call_id") != "call_1" || path(t, tool, "content") != "a.go\nb.go" {
				t.Errorf("tool message: %s", mustJSON(t, tool))
			}
			if h := rc.headers[0].Get("Authorization"); h != "Bearer k" {
				t.Errorf("auth header = %q", h)
			}
		})
	}
}

func TestOpenAIToolChoiceAndCompat(t *testing.T) {
	rc := &recorder{t: t, reply: func(int, http.ResponseWriter) {}}
	rc.reply = func(_ int, w http.ResponseWriter) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	c := newTestClient(t, ModelConfig{Provider: "ollama", Model: "qwen:7b", BaseURL: srv.URL})
	for i, choice := range []ToolChoice{{Mode: ToolRequired}, {Mode: ToolSpecific, Name: "ls"}, {Mode: ToolNone}} {
		_, err := c.Chat(t.Context(), &Request{Messages: []Message{{Role: User, Text: "hi"}}, Tools: []Tool{lsTool}, ToolChoice: choice}, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := mustJSON(t, rc.body(i)["tool_choice"])
		want := []string{`"required"`, `{"function":{"name":"ls"},"type":"function"}`, `"none"`}[i]
		if got != want {
			t.Errorf("choice %v: tool_choice = %s, want %s", choice, got, want)
		}
	}
	if rc.body(0)["max_tokens"] == nil {
		t.Errorf("compat servers should get max_tokens")
	}
	if rc.headers[0].Get("Authorization") != "" {
		t.Errorf("ollama without a key should send no auth header")
	}
}

func TestOpenAIStreamWithoutIndex(t *testing.T) {
	// Ollama-style: whole tool calls, no "index", "stop" finish reason.
	rc := &recorder{t: t}
	rc.reply = func(_ int, w http.ResponseWriter) {
		sse(w,
			`data: {"choices":[{"delta":{"tool_calls":[{"id":"a","function":{"name":"ls","arguments":"{\"path\":\"x\"}"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"id":"b","function":{"name":"ls","arguments":"{\"path\":\"y\"}"}}]}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		)
	}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	c := newTestClient(t, ModelConfig{Provider: "ollama", Model: "m", BaseURL: srv.URL})
	resp, err := c.Chat(t.Context(), &Request{Messages: []Message{{Role: User, Text: "hi"}}, Tools: []Tool{lsTool}}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Message.ToolCalls) != 2 || resp.Message.ToolCalls[1].ID != "b" || string(resp.Message.ToolCalls[1].Input) != `{"path":"y"}` {
		t.Fatalf("calls = %+v", resp.Message.ToolCalls)
	}
	if resp.StopReason != StopToolUse {
		t.Errorf("stop = %s, want tool_use despite finish_reason=stop", resp.StopReason)
	}
}

func TestOpenAIStreamError(t *testing.T) {
	rc := &recorder{t: t}
	rc.reply = func(_ int, w http.ResponseWriter) {
		sse(w, `data: {"error":{"message":"boom","type":"server_error"}}`)
	}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	c := newTestClient(t, ModelConfig{Provider: "openai", Model: "m", APIKey: "k", BaseURL: srv.URL})
	_, err := c.Chat(t.Context(), &Request{Messages: []Message{{Role: User, Text: "hi"}}}, func(Event) {})
	if err == nil || !strings.Contains(err.Error(), "boom") || !IsRetryable(err) {
		t.Fatalf("err = %v (retryable %v)", err, IsRetryable(err))
	}
}

func TestOpenAIBrokenArguments(t *testing.T) {
	rc := &recorder{t: t}
	rc.reply = func(_ int, w http.ResponseWriter) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"tool_calls":[{"function":{"name":"ls","arguments":"{\"path\": "}}]},"finish_reason":"tool_calls"}]}`)
	}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	c := newTestClient(t, ModelConfig{Provider: "openai", Model: "m", APIKey: "k", BaseURL: srv.URL})
	resp, err := c.Chat(t.Context(), &Request{Messages: []Message{{Role: User, Text: "hi"}}, Tools: []Tool{lsTool}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tc := resp.Message.ToolCalls[0]
	if !strings.HasPrefix(tc.ID, "call_") || string(tc.Input) != `{"_raw":"{\"path\":"}` {
		t.Fatalf("call = %+v input=%s", tc, tc.Input)
	}
}
