package llm

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

// bedrockEnv gives the SDK static fake credentials and no shared config.
func bedrockEnv(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDTEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
}

// brEvents writes a ConverseStream response: pairs of event type and JSON payload.
func brEvents(t *testing.T, w http.ResponseWriter, pairs ...string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
	enc := eventstream.NewEncoder()
	var buf bytes.Buffer
	for i := 0; i+1 < len(pairs); i += 2 {
		msg := eventstream.Message{
			Headers: eventstream.Headers{
				{Name: ":message-type", Value: eventstream.StringValue("event")},
				{Name: ":event-type", Value: eventstream.StringValue(pairs[i])},
				{Name: ":content-type", Value: eventstream.StringValue("application/json")},
			},
			Payload: []byte(pairs[i+1]),
		}
		if err := enc.Encode(&buf, msg); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = w.Write(buf.Bytes())
}

func bedrockServer(t *testing.T, stream bool) (*recorder, Client) {
	bedrockEnv(t)
	rc := &recorder{t: t}
	rc.reply = func(n int, w http.ResponseWriter) {
		switch {
		case n == 0 && !stream:
			_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[
				{"reasoningContent":{"reasoningText":{"text":"need ls","signature":"bsig"}}},
				{"toolUse":{"toolUseId":"tooluse_1","name":"ls","input":{"path":"."}}}]}},
				"stopReason":"tool_use","usage":{"inputTokens":3,"outputTokens":5,"totalTokens":8,"cacheReadInputTokens":7,"cacheWriteInputTokens":100}}`)
		case n == 1 && !stream:
			_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"text":"Two files."}]}},"stopReason":"end_turn","usage":{"inputTokens":20,"outputTokens":3,"totalTokens":23}}`)
		case n == 0:
			brEvents(t, w,
				"messageStart", `{"role":"assistant"}`,
				"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"reasoningContent":{"text":"need "}}}`,
				"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"reasoningContent":{"text":"ls"}}}`,
				"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"reasoningContent":{"signature":"bsig"}}}`,
				"contentBlockStop", `{"contentBlockIndex":0}`,
				"contentBlockStart", `{"contentBlockIndex":1,"start":{"toolUse":{"toolUseId":"tooluse_1","name":"ls"}}}`,
				"contentBlockDelta", `{"contentBlockIndex":1,"delta":{"toolUse":{"input":"{\"path\":"}}}`,
				"contentBlockDelta", `{"contentBlockIndex":1,"delta":{"toolUse":{"input":"\".\"}"}}}`,
				"contentBlockStop", `{"contentBlockIndex":1}`,
				"messageStop", `{"stopReason":"tool_use"}`,
				"metadata", `{"usage":{"inputTokens":3,"outputTokens":5,"totalTokens":8,"cacheReadInputTokens":7,"cacheWriteInputTokens":100},"metrics":{"latencyMs":10}}`,
			)
		default:
			brEvents(t, w,
				"messageStart", `{"role":"assistant"}`,
				"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"Two "}}`,
				"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"files."}}`,
				"contentBlockStop", `{"contentBlockIndex":0}`,
				"messageStop", `{"stopReason":"end_turn"}`,
				"metadata", `{"usage":{"inputTokens":20,"outputTokens":3,"totalTokens":23},"metrics":{"latencyMs":10}}`,
			)
		}
	}
	srv := httptest.NewServer(rc)
	t.Cleanup(srv.Close)
	return rc, newTestClient(t, ModelConfig{Provider: "bedrock", Model: "vendor.model-x", Region: "us-east-1", BaseURL: srv.URL})
}

func TestBedrockConversation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stream"}[stream], func(t *testing.T) {
			rc, c := bedrockServer(t, stream)
			first, _, events := conversation(t, c, stream)

			if r := first.Message.Reasoning; len(r) != 1 || r[0].Text != "need ls" || r[0].Signature != "bsig" {
				t.Errorf("reasoning = %+v", r)
			}
			if want := (Usage{InputTokens: 110, OutputTokens: 5, CachedInputTokens: 7, CacheWriteTokens: 100}); first.Usage != want {
				t.Errorf("usage = %+v, want %+v", first.Usage, want)
			}
			if stream && (!hasEvent(events, EventToolStart, "ls") || !hasEvent(events, EventText, "Two") || !hasEvent(events, EventReasoning, "need")) {
				t.Errorf("events = %+v", events)
			}
			wantPath := "/model/vendor.model-x/converse"
			if stream {
				wantPath += "-stream"
			}
			if rc.paths[0] != wantPath {
				t.Errorf("path = %q, want %q", rc.paths[0], wantPath)
			}

			b0 := rc.body(0)
			if path(t, b0, "system", 0, "text") != "You are a test." ||
				path(t, b0, "toolConfig", "tools", 0, "toolSpec", "name") != "ls" ||
				path(t, b0, "toolConfig", "tools", 0, "toolSpec", "inputSchema", "json", "type") != "object" {
				t.Errorf("turn 1 body: %s", mustJSON(t, b0))
			}
			b1 := rc.body(1)
			asst := path(t, b1, "messages", 1, "content")
			if path(t, asst, 0, "reasoningContent", "reasoningText", "signature") != "bsig" ||
				path(t, asst, 1, "toolUse", "toolUseId") != "tooluse_1" ||
				path(t, asst, 1, "toolUse", "input", "path") != "." {
				t.Errorf("assistant content must replay reasoning and toolUse: %s", mustJSON(t, asst))
			}
			res := path(t, b1, "messages", 2, "content", 0, "toolResult")
			if path(t, res, "toolUseId") != "tooluse_1" || path(t, res, "status") != "success" ||
				path(t, res, "content", 0, "text") != "a.go\nb.go" {
				t.Errorf("tool result: %s", mustJSON(t, res))
			}
			if !strings.HasPrefix(rc.headers[0].Get("Authorization"), "AWS4-HMAC-SHA256") {
				t.Errorf("request not SigV4-signed")
			}
		})
	}
}

func TestBedrockDowngrades(t *testing.T) {
	bedrockEnv(t)
	rc := &recorder{t: t}
	validation := func(w http.ResponseWriter, msg string) {
		w.Header().Set("X-Amzn-Errortype", "ValidationException")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"`+msg+`"}`)
	}
	rc.reply = func(n int, w http.ResponseWriter) {
		switch n {
		case 0:
			validation(w, "This model doesn't support the cachePoint field.")
		case 1:
			validation(w, "This model doesn't support the toolConfig.toolChoice.any field.")
		case 2:
			validation(w, "This model doesn't support system messages.")
		default:
			_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"text":"ok"}]}},"stopReason":"end_turn"}`)
		}
	}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	c := newTestClient(t, ModelConfig{Provider: "bedrock", Model: "m", Region: "us-east-1", BaseURL: srv.URL})
	req := &Request{System: "sys", Messages: []Message{{Role: User, Text: "hi"}}, Tools: []Tool{lsTool}, ToolChoice: ToolChoice{Mode: ToolRequired}, Cache: true}
	resp, err := c.Chat(t.Context(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Text != "ok" {
		t.Fatalf("text = %q", resp.Message.Text)
	}
	last := rc.body(3)
	if _, ok := last["system"]; ok {
		t.Errorf("system should have moved into the user turn")
	}
	if text := path(t, last, "messages", 0, "content", 0, "text").(string); !strings.Contains(text, "<instructions>\nsys") {
		t.Errorf("first user text = %q", text)
	}
	if strings.Contains(mustJSON(t, last), "cachePoint") || strings.Contains(mustJSON(t, last), "toolChoice") {
		t.Errorf("cache points and forced choice should be gone: %s", mustJSON(t, last))
	}
	// The downgrades are remembered: the next call goes straight through.
	if _, err := c.Chat(t.Context(), req, nil); err != nil || len(rc.bodies) != 5 {
		t.Errorf("second call: err=%v requests=%d, want 5", err, len(rc.bodies))
	}
}

// Seen live on qwen.qwen3-coder-next: cache points are refused with
// AccessDeniedException rather than ValidationException.
func TestBedrockCacheAccessDenied(t *testing.T) {
	bedrockEnv(t)
	rc := &recorder{t: t}
	rc.reply = func(n int, w http.ResponseWriter) {
		if n == 0 {
			w.Header().Set("X-Amzn-Errortype", "AccessDeniedException")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"You invoked an unsupported model or your request did not allow prompt caching. See the documentation for more information."}`)
			return
		}
		_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"text":"ok"}]}},"stopReason":"end_turn"}`)
	}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	c := newTestClient(t, ModelConfig{Provider: "bedrock", Model: "m", Region: "us-east-1", BaseURL: srv.URL})
	req := &Request{System: "sys", Messages: []Message{{Role: User, Text: "hi"}}, Cache: true}
	if _, err := c.Chat(t.Context(), req, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(mustJSON(t, rc.body(1)), "cachePoint") {
		t.Errorf("retry should drop cache points: %s", mustJSON(t, rc.body(1)))
	}

	// A plain access denial (IAM/SCP) is returned as-is, not retried.
	rc2 := &recorder{t: t}
	rc2.reply = func(_ int, w http.ResponseWriter) {
		w.Header().Set("X-Amzn-Errortype", "AccessDeniedException")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"not authorized to perform: bedrock:InvokeModel"}`)
	}
	srv2 := httptest.NewServer(rc2)
	defer srv2.Close()
	c2 := newTestClient(t, ModelConfig{Provider: "bedrock", Model: "m", Region: "us-east-1", BaseURL: srv2.URL})
	if _, err := c2.Chat(t.Context(), req, nil); err == nil || len(rc2.bodies) != 1 || IsRetryable(err) {
		t.Errorf("err=%v requests=%d retryable=%v", err, len(rc2.bodies), IsRetryable(err))
	}
}

func TestBedrockThrottlingRetryable(t *testing.T) {
	bedrockEnv(t)
	rc := &recorder{t: t}
	rc.reply = func(_ int, w http.ResponseWriter) {
		w.Header().Set("X-Amzn-Errortype", "ThrottlingException")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"message":"slow down"}`)
	}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	c := newTestClient(t, ModelConfig{Provider: "bedrock", Model: "m", Region: "us-east-1", BaseURL: srv.URL})
	_, err := c.Chat(t.Context(), &Request{Messages: []Message{{Role: User, Text: "hi"}}}, nil)
	if err == nil || !IsRetryable(err) {
		t.Fatalf("err = %v, want retryable", err)
	}
}
