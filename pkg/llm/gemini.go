package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
)

func init() {
	Register("gemini", func(cfg ModelConfig, _ *slog.Logger) (Client, error) { return newGemini(cfg) })
}

const geminiProvider = "gemini"

// geminiClient talks to the Gemini API (generativelanguage.googleapis.com).
type geminiClient struct {
	cfg     ModelConfig
	base    string // .../models/<model>
	headers map[string]string
	hc      *http.Client
}

func newGemini(cfg ModelConfig) (*geminiClient, error) {
	key := resolveKey(cfg, "GEMINI_API_KEY", "GOOGLE_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("API key missing: set GEMINI_API_KEY (or GOOGLE_API_KEY) or api_key/api_key_env")
	}
	base := firstNonEmpty(cfg.BaseURL, os.Getenv("GEMINI_BASE_URL"), "https://generativelanguage.googleapis.com/v1beta")
	model := strings.TrimPrefix(cfg.Model, "models/")
	return &geminiClient{
		cfg:     cfg,
		base:    strings.TrimRight(base, "/") + "/models/" + url.PathEscape(model),
		headers: mergeHeaders(map[string]string{"x-goog-api-key": key}, cfg.Headers),
		hc:      newHTTPClient(),
	}, nil
}

func (c *geminiClient) Name() string { return "gemini:" + c.cfg.Model }

// --- wire types ---

type gmFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type gmFunctionResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type gmPart struct {
	Text             string              `json:"text,omitempty"`
	Thought          bool                `json:"thought,omitempty"`
	ThoughtSignature string              `json:"thoughtSignature,omitempty"`
	FunctionCall     *gmFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *gmFunctionResponse `json:"functionResponse,omitempty"`
}

type gmContent struct {
	Role  string   `json:"role,omitempty"`
	Parts []gmPart `json:"parts"`
}

type gmResponse struct {
	Candidates []struct {
		Content      gmContent `json:"content"`
		FinishReason string    `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	UsageMetadata *struct {
		PromptTokenCount        int `json:"promptTokenCount"`
		CandidatesTokenCount    int `json:"candidatesTokenCount"`
		ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
	} `json:"usageMetadata"`
	ModelVersion string `json:"modelVersion"`
	Error        *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// --- request ---

func (c *geminiClient) body(req *Request) map[string]any {
	contents := make([]gmContent, 0, len(req.Messages))
	for _, m := range req.Messages {
		var parts []gmPart
		role := "user"
		switch m.Role {
		case User:
			for _, tr := range m.ToolResults {
				resp := map[string]any{"content": tr.Content}
				if tr.IsError {
					resp = map[string]any{"error": tr.Content}
				}
				parts = append(parts, gmPart{FunctionResponse: &gmFunctionResponse{Name: tr.Name, Response: resp}})
			}
			if m.Text != "" || len(m.ToolResults) == 0 {
				parts = append(parts, gmPart{Text: m.Text})
			}
		case Assistant:
			role = "model"
			if m.Text != "" {
				p := gmPart{Text: m.Text}
				for _, r := range m.Reasoning {
					if r.Provider == geminiProvider && r.Signature != "" {
						p.ThoughtSignature = r.Signature
					}
				}
				parts = append(parts, p)
			}
			for _, tc := range m.ToolCalls {
				parts = append(parts, gmPart{
					FunctionCall:     &gmFunctionCall{Name: tc.Name, Args: toolInput(tc.Input)},
					ThoughtSignature: tc.Signature,
				})
			}
			if len(parts) == 0 {
				parts = append(parts, gmPart{Text: ""})
			}
		}
		contents = append(contents, gmContent{Role: role, Parts: parts})
	}
	gen := map[string]any{"maxOutputTokens": maxTokens(req, c.cfg)}
	if t := firstTemp(req.Temperature, c.cfg.Temperature); t != nil {
		gen["temperature"] = *t
	}
	body := map[string]any{"contents": contents, "generationConfig": gen}
	if req.System != "" {
		body["systemInstruction"] = gmContent{Parts: []gmPart{{Text: req.System}}}
	}
	if len(req.Tools) > 0 {
		decls := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			decls = append(decls, map[string]any{
				"name":                 t.Name,
				"description":          t.Description,
				"parametersJsonSchema": schemaOrEmpty(t.InputSchema),
			})
		}
		body["tools"] = []map[string]any{{"functionDeclarations": decls}}
		fc := map[string]any{}
		switch req.ToolChoice.Mode {
		case ToolNone:
			fc["mode"] = "NONE"
		case ToolRequired:
			fc["mode"] = "ANY"
		case ToolSpecific:
			fc["mode"] = "ANY"
			fc["allowedFunctionNames"] = []string{req.ToolChoice.Name}
		}
		if len(fc) > 0 {
			body["toolConfig"] = map[string]any{"functionCallingConfig": fc}
		}
	}
	return body
}

// --- Chat ---

func (c *geminiClient) Chat(ctx context.Context, req *Request, handler EventHandler) (*Response, error) {
	ctx, cancel := withTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	acc := &gmAccumulator{model: c.cfg.Model}
	if handler == nil {
		var out gmResponse
		if err := postJSON(ctx, c.hc, geminiProvider, c.base+":generateContent", c.headers, c.body(req), &out); err != nil {
			return nil, err
		}
		if err := acc.add(&out, nil); err != nil {
			return nil, err
		}
		return acc.response()
	}
	httpResp, err := post(ctx, c.hc, geminiProvider, c.base+":streamGenerateContent?alt=sse", c.headers, c.body(req))
	if err != nil {
		return nil, err
	}
	defer func() { _ = httpResp.Body.Close() }()
	err = readSSE(httpResp.Body, func(_ string, data []byte) error {
		var out gmResponse
		if err := json.Unmarshal(data, &out); err != nil {
			return fmt.Errorf("gemini: decode stream chunk: %w", err)
		}
		return acc.add(&out, handler)
	})
	if err != nil {
		return nil, err
	}
	return acc.response()
}

// gmAccumulator merges one response, or the chunks of a stream, into a
// reply. Streamed text arrives in pieces; function calls arrive whole.
type gmAccumulator struct {
	text, thought strings.Builder
	textSig       string
	calls         []ToolCall
	finish        string
	model         string
	usage         Usage
	blocked       string
	gotCandidate  bool
}

func (a *gmAccumulator) add(out *gmResponse, handler EventHandler) error {
	if out.Error != nil {
		return &StreamError{Provider: geminiProvider, Type: out.Error.Status, Message: out.Error.Message,
			retryable: out.Error.Code == 429 || out.Error.Code >= 500}
	}
	if out.ModelVersion != "" {
		a.model = out.ModelVersion
	}
	if u := out.UsageMetadata; u != nil {
		a.usage = Usage{
			InputTokens:       u.PromptTokenCount,
			OutputTokens:      u.CandidatesTokenCount + u.ThoughtsTokenCount,
			CachedInputTokens: u.CachedContentTokenCount,
		}
	}
	if out.PromptFeedback.BlockReason != "" {
		a.blocked = out.PromptFeedback.BlockReason
	}
	if len(out.Candidates) == 0 {
		return nil
	}
	a.gotCandidate = true
	cand := out.Candidates[0]
	if cand.FinishReason != "" {
		a.finish = cand.FinishReason
	}
	for _, p := range cand.Content.Parts {
		switch {
		case p.FunctionCall != nil:
			tc := ToolCall{
				ID:        newCallID(),
				Name:      p.FunctionCall.Name,
				Input:     toolInput(p.FunctionCall.Args),
				Signature: p.ThoughtSignature,
			}
			a.calls = append(a.calls, tc)
			emit(handler, Event{Type: EventToolStart, ToolCallID: tc.ID, ToolName: tc.Name})
		case p.Thought:
			a.thought.WriteString(p.Text)
			if p.Text != "" {
				emit(handler, Event{Type: EventReasoning, Text: p.Text})
			}
		default:
			a.text.WriteString(p.Text)
			if p.ThoughtSignature != "" {
				a.textSig = p.ThoughtSignature
			}
			if p.Text != "" {
				emit(handler, Event{Type: EventText, Text: p.Text})
			}
		}
	}
	return nil
}

func (a *gmAccumulator) response() (*Response, error) {
	if !a.gotCandidate {
		if a.blocked != "" {
			return nil, fmt.Errorf("gemini: prompt blocked: %s", a.blocked)
		}
		return nil, fmt.Errorf("gemini: no candidates returned")
	}
	msg := Message{Role: Assistant, Text: a.text.String(), ToolCalls: a.calls}
	if a.thought.Len() > 0 || a.textSig != "" {
		msg.Reasoning = []Reasoning{{Provider: geminiProvider, Text: a.thought.String(), Signature: a.textSig}}
	}
	return &Response{
		Message:    msg,
		StopReason: gmStop(a.finish, len(a.calls) > 0),
		RawStop:    a.finish,
		Model:      a.model,
		Usage:      a.usage,
	}, nil
}

func gmStop(reason string, hasCalls bool) StopReason {
	switch {
	case hasCalls:
		return StopToolUse
	case reason == "STOP" || reason == "":
		return StopEnd
	case reason == "MAX_TOKENS":
		return StopMaxTokens
	default:
		return StopOther // SAFETY, RECITATION, MALFORMED_FUNCTION_CALL, ...
	}
}
