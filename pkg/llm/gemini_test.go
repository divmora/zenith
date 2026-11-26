package llm

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func geminiServer(t *testing.T, stream bool) (*recorder, Client) {
	rc := &recorder{t: t}
	rc.reply = func(n int, w http.ResponseWriter) {
		switch {
		case n == 0 && !stream:
			_, _ = io.WriteString(w, `{"modelVersion":"gemini-x-001","candidates":[{"content":{"role":"model","parts":[
				{"text":"need ls","thought":true},
				{"functionCall":{"name":"ls","args":{"path":"."}},"thoughtSignature":"gsig"}]},"finishReason":"STOP"}],
				"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":2,"cachedContentTokenCount":4}}`)
		case n == 1 && !stream:
			_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"Two files."}]},"finishReason":"STOP"}]}`)
		case n == 0:
			sse(w,
				`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"need ","thought":true}]}}]}`,
				`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"ls","thought":true}]}}]}`,
				`data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"ls","args":{"path":"."}},"thoughtSignature":"gsig"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":2,"cachedContentTokenCount":4},"modelVersion":"gemini-x-001"}`,
			)
		default:
			sse(w,
				`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Two "}]}}]}`,
				`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"files."}]},"finishReason":"STOP"}]}`,
			)
		}
	}
	srv := httptest.NewServer(rc)
	t.Cleanup(srv.Close)
	return rc, newTestClient(t, ModelConfig{Provider: "gemini", Model: "gemini-x", APIKey: "k", BaseURL: srv.URL})
}

func TestGeminiConversation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stream"}[stream], func(t *testing.T) {
			rc, c := geminiServer(t, stream)
			first, _, events := conversation(t, c, stream)

			if sig := first.Message.ToolCalls[0].Signature; sig != "gsig" {
				t.Errorf("call signature = %q", sig)
			}
			if r := first.Message.Reasoning; len(r) != 1 || r[0].Text != "need ls" {
				t.Errorf("reasoning = %+v", r)
			}
			if want := (Usage{InputTokens: 10, OutputTokens: 7, CachedInputTokens: 4}); first.Usage != want {
				t.Errorf("usage = %+v, want %+v (output includes thoughts)", first.Usage, want)
			}
			if first.Model != "gemini-x-001" {
				t.Errorf("model = %q", first.Model)
			}
			if stream && (!hasEvent(events, EventReasoning, "need") || !hasEvent(events, EventText, "files")) {
				t.Errorf("events = %+v", events)
			}

			wantPath := "/models/gemini-x:generateContent"
			if stream {
				wantPath = "/models/gemini-x:streamGenerateContent?alt=sse"
			}
			if rc.paths[0] != wantPath {
				t.Errorf("path = %q, want %q", rc.paths[0], wantPath)
			}
			b0 := rc.body(0)
			if path(t, b0, "systemInstruction", "parts", 0, "text") != "You are a test." ||
				path(t, b0, "tools", 0, "functionDeclarations", 0, "parametersJsonSchema", "type") != "object" {
				t.Errorf("turn 1 body: %s", mustJSON(t, b0))
			}
			if _, ok := b0["toolConfig"]; ok {
				t.Errorf("auto should not send toolConfig")
			}
			b1 := rc.body(1)
			call := path(t, b1, "contents", 1)
			if path(t, call, "role") != "model" || path(t, call, "parts", 0, "functionCall", "name") != "ls" ||
				path(t, call, "parts", 0, "thoughtSignature") != "gsig" {
				t.Errorf("model turn must replay the call with its signature: %s", mustJSON(t, call))
			}
			res := path(t, b1, "contents", 2, "parts", 0, "functionResponse")
			if path(t, res, "name") != "ls" || path(t, res, "response", "content") != "a.go\nb.go" {
				t.Errorf("function response: %s", mustJSON(t, res))
			}
			if rc.headers[0].Get("x-goog-api-key") != "k" {
				t.Errorf("api key header missing")
			}
		})
	}
}

func TestGeminiToolChoiceAndErrors(t *testing.T) {
	rc := &recorder{t: t}
	rc.reply = func(n int, w http.ResponseWriter) {
		switch n {
		case 0:
			_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
		case 1:
			_, _ = io.WriteString(w, `{"promptFeedback":{"blockReason":"SAFETY"}}`)
		default:
			sse(w, `data: {"error":{"code":503,"message":"overloaded","status":"UNAVAILABLE"}}`)
		}
	}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	c := newTestClient(t, ModelConfig{Provider: "gemini", Model: "m", APIKey: "k", BaseURL: srv.URL})
	req := &Request{Messages: []Message{{Role: User, Text: "hi"}}, Tools: []Tool{lsTool}, ToolChoice: ToolChoice{Mode: ToolSpecific, Name: "ls"}}
	if _, err := c.Chat(t.Context(), req, nil); err != nil {
		t.Fatal(err)
	}
	fc := path(t, rc.body(0), "toolConfig", "functionCallingConfig")
	if path(t, fc, "mode") != "ANY" || path(t, fc, "allowedFunctionNames", 0) != "ls" {
		t.Errorf("functionCallingConfig = %s", mustJSON(t, fc))
	}
	if _, err := c.Chat(t.Context(), req, nil); err == nil || !strings.Contains(err.Error(), "SAFETY") {
		t.Errorf("blocked prompt: err = %v", err)
	}
	_, err := c.Chat(t.Context(), req, func(Event) {})
	if err == nil || !IsRetryable(err) {
		t.Errorf("in-stream 503 should be a retryable error, got %v", err)
	}
}
