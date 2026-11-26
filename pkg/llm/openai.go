package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
)

// openAICompat describes an OpenAI-compatible service.
type openAICompat struct {
	baseURL     string   // default base URL ("" = must be configured)
	keyEnvs     []string // env vars tried for the API key
	baseEnvs    []string // env vars tried for the base URL
	keyOptional bool
	azure       bool // send "api-key" header instead of bearer
	modern      bool // uses max_completion_tokens (OpenAI, Azure)
}

var compatProviders = map[string]openAICompat{
	"openai":            {baseURL: "https://api.openai.com/v1", keyEnvs: []string{"OPENAI_API_KEY"}, baseEnvs: []string{"OPENAI_BASE_URL"}, modern: true},
	"azure":             {keyEnvs: []string{"AZURE_OPENAI_API_KEY"}, baseEnvs: []string{"AZURE_OPENAI_BASE_URL"}, azure: true, modern: true},
	"ollama":            {baseURL: "http://localhost:11434/v1", baseEnvs: []string{"OLLAMA_BASE_URL"}, keyOptional: true},
	"openrouter":        {baseURL: "https://openrouter.ai/api/v1", keyEnvs: []string{"OPENROUTER_API_KEY"}},
	"groq":              {baseURL: "https://api.groq.com/openai/v1", keyEnvs: []string{"GROQ_API_KEY"}},
	"deepseek":          {baseURL: "https://api.deepseek.com/v1", keyEnvs: []string{"DEEPSEEK_API_KEY"}},
	"mistral":           {baseURL: "https://api.mistral.ai/v1", keyEnvs: []string{"MISTRAL_API_KEY"}},
	"xai":               {baseURL: "https://api.x.ai/v1", keyEnvs: []string{"XAI_API_KEY"}},
	"together":          {baseURL: "https://api.together.xyz/v1", keyEnvs: []string{"TOGETHER_API_KEY"}},
	"litellm":           {baseURL: "http://localhost:4000/v1", keyEnvs: []string{"LITELLM_API_KEY"}, baseEnvs: []string{"LITELLM_BASE_URL"}, keyOptional: true},
	"openai-compatible": {keyEnvs: []string{"OPENAI_COMPATIBLE_API_KEY"}, baseEnvs: []string{"OPENAI_COMPATIBLE_BASE_URL"}, keyOptional: true},
}

func init() {
	for name, spec := range compatProviders {
		Register(name, func(cfg ModelConfig, _ *slog.Logger) (Client, error) {
			return newOpenAI(name, spec, cfg)
		})
	}
}

type openAIClient struct {
	provider string
	spec     openAICompat
	cfg      ModelConfig
	url      string
	headers  map[string]string
	hc       *http.Client
}

func newOpenAI(name string, spec openAICompat, cfg ModelConfig) (*openAIClient, error) {
	base := cfg.BaseURL
	for _, e := range spec.baseEnvs {
		if base != "" {
			break
		}
		base = os.Getenv(e)
	}
	if base == "" {
		base = spec.baseURL
	}
	if base == "" {
		return nil, fmt.Errorf("base_url is required (or set %s)", strings.Join(spec.baseEnvs, "/"))
	}
	key := resolveKey(cfg, spec.keyEnvs...)
	if key == "" && !spec.keyOptional {
		return nil, fmt.Errorf("API key missing: set %s or api_key/api_key_env", strings.Join(spec.keyEnvs, "/"))
	}
	headers := map[string]string{}
	if key != "" {
		if spec.azure {
			headers["api-key"] = key
		} else {
			headers["Authorization"] = "Bearer " + key
		}
	}
	return &openAIClient{
		provider: name,
		spec:     spec,
		cfg:      cfg,
		url:      strings.TrimRight(base, "/") + "/chat/completions",
		headers:  mergeHeaders(headers, cfg.Headers),
		hc:       newHTTPClient(),
	}, nil
}

func (c *openAIClient) Name() string { return c.provider + ":" + c.cfg.Model }

// --- wire types ---

type oaToolCall struct {
	Index    *int   `json:"index,omitempty"` // streaming only
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaMessage struct {
	Role       string       `json:"role"`
	Content    *string      `json:"content,omitempty"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u *oaUsage) usage() Usage {
	if u == nil {
		return Usage{}
	}
	out := Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}
	if u.PromptTokensDetails != nil {
		out.CachedInputTokens = u.PromptTokensDetails.CachedTokens
	}
	return out
}

type oaReplyMessage struct {
	Content          string       `json:"content"`
	Refusal          string       `json:"refusal"`
	ReasoningContent string       `json:"reasoning_content"` // DeepSeek, vLLM, others
	Reasoning        string       `json:"reasoning"`         // OpenRouter, Groq, Ollama
	ToolCalls        []oaToolCall `json:"tool_calls"`
}

type oaResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      oaReplyMessage `json:"message"`
		FinishReason string         `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaUsage `json:"usage"`
}

type oaChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta        oaReplyMessage `json:"delta"`
		FinishReason string         `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// --- request ---

func (c *openAIClient) body(req *Request, stream bool) map[string]any {
	var msgs []oaMessage
	if req.System != "" {
		s := req.System
		msgs = append(msgs, oaMessage{Role: "system", Content: &s})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case User:
			// Tool results must directly follow the assistant message
			// that asked for them, so they go before any user text.
			for _, tr := range m.ToolResults {
				content := tr.Content
				if tr.IsError {
					content = "Error: " + content
				}
				msgs = append(msgs, oaMessage{Role: "tool", ToolCallID: tr.CallID, Content: &content})
			}
			if m.Text != "" || len(m.ToolResults) == 0 {
				t := m.Text
				msgs = append(msgs, oaMessage{Role: "user", Content: &t})
			}
		case Assistant:
			om := oaMessage{Role: "assistant"}
			if m.Text != "" || len(m.ToolCalls) == 0 {
				t := m.Text
				om.Content = &t
			}
			for _, tc := range m.ToolCalls {
				otc := oaToolCall{ID: tc.ID, Type: "function"}
				otc.Function.Name = tc.Name
				otc.Function.Arguments = string(toolInput(tc.Input))
				om.ToolCalls = append(om.ToolCalls, otc)
			}
			msgs = append(msgs, om)
		}
	}
	body := map[string]any{"model": c.cfg.Model, "messages": msgs}
	if c.spec.modern {
		body["max_completion_tokens"] = maxTokens(req, c.cfg)
	} else {
		body["max_tokens"] = maxTokens(req, c.cfg)
	}
	if t := firstTemp(req.Temperature, c.cfg.Temperature); t != nil {
		body["temperature"] = *t
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  schemaOrEmpty(t.InputSchema),
				},
			})
		}
		body["tools"] = tools
		// "auto" is the default; leaving it out keeps servers that don't
		// know tool_choice working.
		switch req.ToolChoice.Mode {
		case ToolNone:
			body["tool_choice"] = "none"
		case ToolRequired:
			body["tool_choice"] = "required"
		case ToolSpecific:
			body["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": req.ToolChoice.Name}}
		}
	}
	if stream {
		body["stream"] = true
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	return body
}

// --- Chat ---

func (c *openAIClient) Chat(ctx context.Context, req *Request, handler EventHandler) (*Response, error) {
	ctx, cancel := withTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	if handler == nil {
		return c.complete(ctx, req)
	}
	return c.stream(ctx, req, handler)
}

func (c *openAIClient) complete(ctx context.Context, req *Request) (*Response, error) {
	var out oaResponse
	if err := postJSON(ctx, c.hc, c.provider, c.url, c.headers, c.body(req, false), &out); err != nil {
		return nil, err
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("%s: empty choices", c.provider)
	}
	ch := out.Choices[0]
	if ch.Message.Content == "" && ch.Message.Refusal != "" && len(ch.Message.ToolCalls) == 0 {
		return nil, fmt.Errorf("%s: model refused: %s", c.provider, ch.Message.Refusal)
	}
	msg := Message{Role: Assistant, Text: ch.Message.Content}
	if r := firstNonEmpty(ch.Message.ReasoningContent, ch.Message.Reasoning); r != "" {
		msg.Reasoning = []Reasoning{{Provider: c.provider, Text: r}}
	}
	for _, tc := range ch.Message.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			ID:    firstNonEmpty(tc.ID, newCallID()),
			Name:  tc.Function.Name,
			Input: toolInput([]byte(tc.Function.Arguments)),
		})
	}
	return &Response{
		Message:    msg,
		StopReason: oaStop(ch.FinishReason, len(msg.ToolCalls) > 0),
		RawStop:    ch.FinishReason,
		Model:      firstNonEmpty(out.Model, c.cfg.Model),
		Usage:      out.Usage.usage(),
	}, nil
}

type oaPartialCall struct {
	id, name string
	args     strings.Builder
	started  bool
}

func (c *openAIClient) stream(ctx context.Context, req *Request, handler EventHandler) (*Response, error) {
	httpResp, err := post(ctx, c.hc, c.provider, c.url, c.headers, c.body(req, true))
	if err != nil {
		return nil, err
	}
	defer func() { _ = httpResp.Body.Close() }()

	var (
		text, reasoning strings.Builder
		calls           = map[int]*oaPartialCall{}
		finish, model   string
		usage           Usage
	)
	err = readSSE(httpResp.Body, func(_ string, data []byte) error {
		if string(data) == "[DONE]" {
			return errStopSSE
		}
		var ch oaChunk
		if err := json.Unmarshal(data, &ch); err != nil {
			return fmt.Errorf("%s: decode stream chunk: %w", c.provider, err)
		}
		if ch.Error != nil {
			return &StreamError{Provider: c.provider, Type: ch.Error.Type, Message: ch.Error.Message, retryable: ch.Error.Type == "server_error"}
		}
		if ch.Model != "" {
			model = ch.Model
		}
		if ch.Usage != nil {
			usage = ch.Usage.usage()
		}
		for _, choice := range ch.Choices {
			d := choice.Delta
			if d.Content != "" {
				text.WriteString(d.Content)
				handler(Event{Type: EventText, Text: d.Content})
			}
			if r := firstNonEmpty(d.ReasoningContent, d.Reasoning); r != "" {
				reasoning.WriteString(r)
				handler(Event{Type: EventReasoning, Text: r})
			}
			for j, tc := range d.ToolCalls {
				idx := j
				if tc.Index != nil {
					idx = *tc.Index
				} else if prev := calls[idx]; prev != nil && tc.ID != "" && prev.id != tc.ID {
					// Servers without "index" (Ollama) send each call
					// whole; a new id means a new call.
					idx = len(calls)
				}
				pc := calls[idx]
				if pc == nil {
					pc = &oaPartialCall{}
					calls[idx] = pc
				}
				if tc.ID != "" {
					pc.id = tc.ID
				}
				if tc.Function.Name != "" {
					pc.name += tc.Function.Name
				}
				pc.args.WriteString(tc.Function.Arguments)
				if !pc.started && pc.name != "" {
					pc.started = true
					if pc.id == "" {
						pc.id = newCallID()
					}
					handler(Event{Type: EventToolStart, ToolCallID: pc.id, ToolName: pc.name})
				}
			}
			if choice.FinishReason != "" {
				finish = choice.FinishReason
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	msg := Message{Role: Assistant, Text: text.String()}
	if reasoning.Len() > 0 {
		msg.Reasoning = []Reasoning{{Provider: c.provider, Text: reasoning.String()}}
	}
	idxs := make([]int, 0, len(calls))
	for i := range calls {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		pc := calls[i]
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			ID:    firstNonEmpty(pc.id, newCallID()),
			Name:  pc.name,
			Input: toolInput([]byte(pc.args.String())),
		})
	}
	return &Response{
		Message:    msg,
		StopReason: oaStop(finish, len(msg.ToolCalls) > 0),
		RawStop:    finish,
		Model:      firstNonEmpty(model, c.cfg.Model),
		Usage:      usage,
	}, nil
}

// oaStop normalises a finish_reason. Some servers report "stop" even when
// the reply has tool calls, so the calls decide.
func oaStop(finish string, hasCalls bool) StopReason {
	switch {
	case hasCalls:
		return StopToolUse
	case finish == "stop" || finish == "":
		return StopEnd
	case finish == "length":
		return StopMaxTokens
	case finish == "tool_calls" || finish == "function_call":
		return StopToolUse
	default:
		return StopOther
	}
}
