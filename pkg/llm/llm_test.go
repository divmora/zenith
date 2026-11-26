package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func init() { retryBase = time.Millisecond }

func TestParseSpec(t *testing.T) {
	cases := map[string]ModelConfig{
		"openai:gpt-x":                    {Provider: "openai", Model: "gpt-x"},
		"Bedrock:anthropic.claude-x-v1:0": {Provider: "bedrock", Model: "anthropic.claude-x-v1:0"},
		"ollama:qwen2.5-coder:14b":        {Provider: "ollama", Model: "qwen2.5-coder:14b"},
	}
	for spec, want := range cases {
		got, err := ParseSpec(spec)
		if err != nil || got.Provider != want.Provider || got.Model != want.Model {
			t.Errorf("ParseSpec(%q) = %+v, %v", spec, got, err)
		}
	}
	for _, bad := range []string{"", "openai", ":m", "openai:"} {
		if _, err := ParseSpec(bad); err == nil {
			t.Errorf("ParseSpec(%q) should fail", bad)
		}
	}
}

func TestProvidersRegistered(t *testing.T) {
	for _, p := range []string{"openai", "anthropic", "gemini", "bedrock", "azure", "ollama", "openrouter", "groq", "deepseek", "mistral", "xai", "together", "litellm", "openai-compatible"} {
		if !Known(p) {
			t.Errorf("provider %q not registered", p)
		}
	}
	if _, err := New(ModelConfig{Provider: "nope", Model: "m"}, quiet); err == nil {
		t.Error("unknown provider should fail")
	}
	t.Setenv("OPENAI_API_KEY", "")
	if _, err := New(ModelConfig{Provider: "openai", Model: "m"}, quiet); err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Errorf("missing key error = %v", err)
	}
}

func TestValidate(t *testing.T) {
	user := []Message{{Role: User, Text: "hi"}}
	bad := map[string]*Request{
		"no messages":      {},
		"dup tools":        {Messages: user, Tools: []Tool{lsTool, lsTool}},
		"unknown specific": {Messages: user, Tools: []Tool{lsTool}, ToolChoice: ToolChoice{Mode: ToolSpecific, Name: "rm"}},
		"required no tool": {Messages: user, ToolChoice: ToolChoice{Mode: ToolRequired}},
		"bad role":         {Messages: []Message{{Role: "system", Text: "x"}}},
		"user calls":       {Messages: []Message{{Role: User, ToolCalls: []ToolCall{{ID: "a", Name: "ls"}}}}},
		"result no id":     {Messages: []Message{{Role: User, ToolResults: []ToolResult{{Content: "x"}}}}},
	}
	for name, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	ok := &Request{Messages: user, Tools: []Tool{lsTool}, ToolChoice: ToolChoice{Mode: ToolSpecific, Name: "ls"}}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid request: %v", err)
	}
}

// flaky fails n times with err, then answers.
type flaky struct {
	n      int
	err    error
	calls  int
	events bool // emit an event before failing
}

func (f *flaky) Name() string { return "flaky" }
func (f *flaky) Chat(_ context.Context, _ *Request, h EventHandler) (*Response, error) {
	f.calls++
	if f.calls <= f.n {
		if f.events && h != nil {
			h(Event{Type: EventText, Text: "partial"})
		}
		return nil, f.err
	}
	return &Response{Message: Message{Role: Assistant, Text: "ok"}, StopReason: StopEnd}, nil
}

func TestRetry(t *testing.T) {
	req := &Request{Messages: []Message{{Role: User, Text: "hi"}}}
	retryable := &APIError{Provider: "x", StatusCode: 429}

	f := &flaky{n: 2, err: retryable}
	if _, err := WithRetry(f, 3, quiet).Chat(t.Context(), req, nil); err != nil || f.calls != 3 {
		t.Errorf("429 x2: err=%v calls=%d", err, f.calls)
	}
	f = &flaky{n: 1, err: &APIError{Provider: "x", StatusCode: 400}}
	if _, err := WithRetry(f, 3, quiet).Chat(t.Context(), req, nil); err == nil || f.calls != 1 {
		t.Errorf("400 must not retry: err=%v calls=%d", err, f.calls)
	}
	f = &flaky{n: 1, err: retryable, events: true}
	if _, err := WithRetry(f, 3, quiet).Chat(t.Context(), req, func(Event) {}); err == nil || f.calls != 1 {
		t.Errorf("failure after output must not retry: err=%v calls=%d", err, f.calls)
	}
	f = &flaky{n: 1, err: retryable, events: true}
	if _, err := WithRetry(f, 3, quiet).Chat(t.Context(), req, nil); err != nil || f.calls != 2 {
		t.Errorf("non-streaming retry: err=%v calls=%d", err, f.calls)
	}
	if _, err := WithRetry(&flaky{}, 0, quiet).Chat(t.Context(), &Request{}, nil); err == nil {
		t.Error("invalid request should fail before sending")
	}
}

func TestRetryAfterHeader(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	c, err := New(ModelConfig{Provider: "openai", Model: "m", APIKey: "k", BaseURL: srv.URL, MaxRetries: 2}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Chat(t.Context(), &Request{Messages: []Message{{Role: User, Text: "hi"}}}, nil); err != nil || calls != 2 {
		t.Errorf("err=%v calls=%d", err, calls)
	}
}

func TestFallback(t *testing.T) {
	req := &Request{Messages: []Message{{Role: User, Text: "hi"}}}
	a := &flaky{n: 9, err: errors.New("down")}
	b := &flaky{}
	fb := &fallback{clients: []Client{a, b}, logger: quiet}
	resp, err := fb.Chat(t.Context(), req, nil)
	if err != nil || resp.Message.Text != "ok" || b.calls != 1 {
		t.Errorf("fallback: resp=%v err=%v", resp, err)
	}
	if fb.Name() != "flaky -> flaky" {
		t.Errorf("name = %q", fb.Name())
	}
	a = &flaky{n: 9, err: errors.New("down"), events: true}
	b = &flaky{}
	fb = &fallback{clients: []Client{a, b}, logger: quiet}
	if _, err := fb.Chat(t.Context(), req, func(Event) {}); err == nil || b.calls != 0 {
		t.Errorf("no fallback after partial output: err=%v b.calls=%d", err, b.calls)
	}
}

func TestCompleteJSON(t *testing.T) {
	rc := &recorder{t: t}
	rc.reply = func(n int, w http.ResponseWriter) {
		if n == 0 {
			_, _ = io.WriteString(w, `{"stop_reason":"tool_use","content":[{"type":"tool_use","id":"t","name":"verdict","input":{"ok":true,"n":12345678901234}}]}`)
			return
		}
		_, _ = io.WriteString(w, "{\"stop_reason\":\"end_turn\",\"content\":[{\"type\":\"text\",\"text\":\"Here:\\n```json\\n{\\\"ok\\\":false}\\n```\"}]}")
	}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	c := newTestClient(t, ModelConfig{Provider: "anthropic", Model: "m", APIKey: "k", BaseURL: srv.URL})
	req := &Request{Messages: []Message{{Role: User, Text: "judge"}}, Tools: []Tool{lsTool}}
	schema := Schema{Name: "verdict", JSON: map[string]any{"type": "object"}}
	raw, _, err := CompleteJSON(t.Context(), c, req, schema)
	if err != nil || string(raw) != `{"ok":true,"n":12345678901234}` {
		t.Fatalf("raw=%s err=%v", raw, err)
	}
	b := rc.body(0)
	if path(t, b, "tool_choice", "name") != "verdict" || len(b["tools"].([]any)) != 1 {
		t.Errorf("forced tool: %s", mustJSON(t, b))
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "ls" {
		t.Errorf("caller's request was modified")
	}
	raw, _, err = CompleteJSON(t.Context(), c, req, schema)
	if err != nil || string(raw) != `{"ok":false}` {
		t.Errorf("text fallback: raw=%s err=%v", raw, err)
	}
}

func TestReadSSE(t *testing.T) {
	in := ": keep-alive\n\nevent: a\ndata: 1\ndata: 2\n\ndata: {\"x\":1}\r\n\r\ndata: tail"
	var got []string
	err := readSSE(strings.NewReader(in), func(ev string, d []byte) error {
		got = append(got, ev+"|"+string(d))
		return nil
	})
	want := []string{"a|1\n2", `|{"x":1}`, "|tail"}
	if err != nil || strings.Join(got, ";") != strings.Join(want, ";") {
		t.Errorf("got %q err=%v, want %q", got, err, want)
	}
	n := 0
	_ = readSSE(strings.NewReader("data: 1\n\ndata: 2\n\n"), func(string, []byte) error {
		n++
		return errStopSSE
	})
	if n != 1 {
		t.Errorf("errStopSSE should stop reading, got %d events", n)
	}
}

func TestRedirectToOtherHostRefused(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("key leaked to other host: %v", r.Header)
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(other.URL, "127.0.0.1", "localhost", 1)+"/x", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c := newTestClient(t, ModelConfig{Provider: "anthropic", Model: "m", APIKey: "secret", BaseURL: srv.URL})
	if _, err := c.Chat(t.Context(), &Request{Messages: []Message{{Role: User, Text: "hi"}}}, nil); err == nil || !strings.Contains(err.Error(), "refusing a redirect") {
		t.Errorf("err = %v", err)
	}
}

func TestTimeoutCoversStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // never finish
	}))
	defer srv.Close()
	c := newTestClient(t, ModelConfig{Provider: "openai", Model: "m", APIKey: "k", BaseURL: srv.URL, Timeout: 100 * time.Millisecond})
	start := time.Now()
	_, err := c.Chat(t.Context(), &Request{Messages: []Message{{Role: User, Text: "hi"}}}, func(Event) {})
	if err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("stalled stream should time out: err=%v after %v", err, time.Since(start))
	}
}
