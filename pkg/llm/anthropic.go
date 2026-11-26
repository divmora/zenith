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

func init() {
	Register("anthropic", func(cfg ModelConfig, _ *slog.Logger) (Client, error) { return newAnthropic(cfg) })
}

// anthropicClient talks to the Anthropic Messages API.
type anthropicClient struct {
	cfg     ModelConfig
	url     string
	headers map[string]string
	hc      *http.Client
}

func newAnthropic(cfg ModelConfig) (*anthropicClient, error) {
	key := resolveKey(cfg, "ANTHROPIC_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("API key missing: set ANTHROPIC_API_KEY or api_key/api_key_env")
	}
	base := firstNonEmpty(cfg.BaseURL, os.Getenv("ANTHROPIC_BASE_URL"), "https://api.anthropic.com")
	return &anthropicClient{
		cfg: cfg,
		url: strings.TrimRight(base, "/") + "/v1/messages",
		headers: mergeHeaders(map[string]string{
			"x-api-key":         key,
			"anthropic-version": "2023-06-01",
		}, cfg.Headers),
		hc: newHTTPClient(),
	}, nil
}

func (c *anthropicClient) Name() string { return "anthropic:" + c.cfg.Model }

const anthropicProvider = "anthropic"

// --- wire types ---

type anBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

type anUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

func (u anUsage) usage() Usage {
	return Usage{
		InputTokens:       u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens,
		OutputTokens:      u.OutputTokens,
		CachedInputTokens: u.CacheReadInputTokens,
		CacheWriteTokens:  u.CacheCreationInputTokens,
	}
}

type anResponse struct {
	Model      string    `json:"model"`
	StopReason string    `json:"stop_reason"`
	Content    []anBlock `json:"content"`
	Usage      anUsage   `json:"usage"`
}

var ephemeral = map[string]any{"type": "ephemeral"}

// --- request ---

func (c *anthropicClient) body(req *Request, stream bool) map[string]any {
	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		var blocks []map[string]any
		switch m.Role {
		case User:
			// Tool results must come first in the user turn.
			for _, tr := range m.ToolResults {
				b := map[string]any{"type": "tool_result", "tool_use_id": tr.CallID, "content": tr.Content}
				if tr.IsError {
					b["is_error"] = true
				}
				blocks = append(blocks, b)
			}
			if m.Text != "" || len(m.ToolResults) == 0 {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Text})
			}
		case Assistant:
			for _, r := range m.Reasoning {
				if r.Provider != anthropicProvider {
					continue
				}
				if r.Redacted != "" {
					blocks = append(blocks, map[string]any{"type": "redacted_thinking", "data": r.Redacted})
				} else {
					blocks = append(blocks, map[string]any{"type": "thinking", "thinking": r.Text, "signature": r.Signature})
				}
			}
			if m.Text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Text})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": inputObject(tc.Input)})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, map[string]any{"type": "text", "text": ""})
			}
		}
		msgs = append(msgs, map[string]any{"role": string(m.Role), "content": blocks})
	}
	if req.Cache && len(msgs) > 0 {
		// Cache the whole conversation so far: the next turn reads it.
		blocks := msgs[len(msgs)-1]["content"].([]map[string]any)
		blocks[len(blocks)-1]["cache_control"] = ephemeral
	}

	body := map[string]any{"model": c.cfg.Model, "max_tokens": maxTokens(req, c.cfg), "messages": msgs}
	if req.System != "" {
		sys := map[string]any{"type": "text", "text": req.System}
		if req.Cache {
			sys["cache_control"] = ephemeral
		}
		body["system"] = []map[string]any{sys}
	}
	if t := firstTemp(req.Temperature, c.cfg.Temperature); t != nil {
		body["temperature"] = *t
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "input_schema": schemaOrEmpty(t.InputSchema)})
		}
		if req.Cache {
			tools[len(tools)-1]["cache_control"] = ephemeral
		}
		body["tools"] = tools
		switch req.ToolChoice.Mode {
		case ToolNone:
			body["tool_choice"] = map[string]any{"type": "none"}
		case ToolRequired:
			body["tool_choice"] = map[string]any{"type": "any"}
		case ToolSpecific:
			body["tool_choice"] = map[string]any{"type": "tool", "name": req.ToolChoice.Name}
		}
	}
	if stream {
		body["stream"] = true
	}
	return body
}

// --- Chat ---

func (c *anthropicClient) Chat(ctx context.Context, req *Request, handler EventHandler) (*Response, error) {
	ctx, cancel := withTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	if handler == nil {
		var out anResponse
		if err := postJSON(ctx, c.hc, anthropicProvider, c.url, c.headers, c.body(req, false), &out); err != nil {
			return nil, err
		}
		return c.response(out), nil
	}
	return c.stream(ctx, req, handler)
}

func (c *anthropicClient) response(out anResponse) *Response {
	msg := Message{Role: Assistant}
	var text strings.Builder
	for _, b := range out.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "thinking":
			msg.Reasoning = append(msg.Reasoning, Reasoning{Provider: anthropicProvider, Text: b.Thinking, Signature: b.Signature})
		case "redacted_thinking":
			msg.Reasoning = append(msg.Reasoning, Reasoning{Provider: anthropicProvider, Redacted: b.Data})
		case "tool_use":
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Input: toolInput(b.Input)})
		}
	}
	msg.Text = text.String()
	return &Response{
		Message:    msg,
		StopReason: anStop(out.StopReason, len(msg.ToolCalls) > 0),
		RawStop:    out.StopReason,
		Model:      firstNonEmpty(out.Model, c.cfg.Model),
		Usage:      out.Usage.usage(),
	}
}

type anStreamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Model string  `json:"model"`
		Usage anUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *anBlock `json:"content_block"`
	Delta        *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type anPartial struct {
	block anBlock
	text  strings.Builder // text, thinking, or tool input JSON
}

func (c *anthropicClient) stream(ctx context.Context, req *Request, handler EventHandler) (*Response, error) {
	httpResp, err := post(ctx, c.hc, anthropicProvider, c.url, c.headers, c.body(req, true))
	if err != nil {
		return nil, err
	}
	defer func() { _ = httpResp.Body.Close() }()

	out := anResponse{}
	blocks := map[int]*anPartial{}
	err = readSSE(httpResp.Body, func(_ string, data []byte) error {
		var ev anStreamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("anthropic: decode stream event: %w", err)
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				out.Model = ev.Message.Model
				out.Usage = ev.Message.Usage
			}
		case "content_block_start":
			if ev.ContentBlock == nil {
				return nil
			}
			p := &anPartial{block: *ev.ContentBlock}
			p.block.Input = nil
			switch p.block.Type {
			case "text":
				p.text.WriteString(p.block.Text)
			case "thinking":
				p.text.WriteString(p.block.Thinking)
			case "tool_use":
				handler(Event{Type: EventToolStart, ToolCallID: p.block.ID, ToolName: p.block.Name})
			}
			blocks[ev.Index] = p
		case "content_block_delta":
			p := blocks[ev.Index]
			if p == nil || ev.Delta == nil {
				return nil
			}
			switch ev.Delta.Type {
			case "text_delta":
				p.text.WriteString(ev.Delta.Text)
				handler(Event{Type: EventText, Text: ev.Delta.Text})
			case "thinking_delta":
				p.text.WriteString(ev.Delta.Thinking)
				handler(Event{Type: EventReasoning, Text: ev.Delta.Thinking})
			case "signature_delta":
				p.block.Signature += ev.Delta.Signature
			case "input_json_delta":
				p.text.WriteString(ev.Delta.PartialJSON)
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				out.StopReason = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				// Counts here are cumulative; input counts may be updated.
				out.Usage.OutputTokens = ev.Usage.OutputTokens
				if ev.Usage.InputTokens > 0 {
					out.Usage.InputTokens = ev.Usage.InputTokens
				}
				if ev.Usage.CacheReadInputTokens > 0 {
					out.Usage.CacheReadInputTokens = ev.Usage.CacheReadInputTokens
				}
				if ev.Usage.CacheCreationInputTokens > 0 {
					out.Usage.CacheCreationInputTokens = ev.Usage.CacheCreationInputTokens
				}
			}
		case "message_stop":
			return errStopSSE
		case "error":
			if ev.Error != nil {
				return &StreamError{
					Provider:  anthropicProvider,
					Type:      ev.Error.Type,
					Message:   ev.Error.Message,
					retryable: ev.Error.Type == "overloaded_error" || ev.Error.Type == "api_error",
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	idxs := make([]int, 0, len(blocks))
	for i := range blocks {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		p := blocks[i]
		b := p.block
		switch b.Type {
		case "text":
			b.Text = p.text.String()
		case "thinking":
			b.Thinking = p.text.String()
		case "tool_use":
			b.Input = json.RawMessage(p.text.String())
		}
		out.Content = append(out.Content, b)
	}
	return c.response(out), nil
}

func anStop(reason string, hasCalls bool) StopReason {
	switch {
	case hasCalls || reason == "tool_use":
		return StopToolUse
	case reason == "end_turn" || reason == "stop_sequence" || reason == "":
		return StopEnd
	case reason == "max_tokens":
		return StopMaxTokens
	default:
		return StopOther // refusal, pause_turn, ...
	}
}
