// Package llm is a provider-neutral client for chat models with tool calling
// and streaming.
//
// Every backend (OpenAI and compatible servers, Anthropic, Google Gemini,
// AWS Bedrock) implements Client. Callers pick a backend with a ModelConfig,
// usually parsed from a spec string such as "bedrock:<model-id>", so
// switching providers is a config change.
//
// A conversation is a list of Messages. The assistant's reply comes back as a
// Message too: append it to the history as-is (including its Reasoning, which
// some providers require to be sent back), run the tool calls it asks for,
// and append one user Message carrying the ToolResults.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Role of a chat message. The system prompt is passed separately.
type Role string

const (
	User      Role = "user"
	Assistant Role = "assistant"
)

// Message is one chat turn.
//
// A user message carries Text and/or ToolResults. An assistant message
// carries Text, ToolCalls and, for reasoning models, Reasoning.
type Message struct {
	Role        Role         `json:"role"`
	Text        string       `json:"text,omitempty"`
	Reasoning   []Reasoning  `json:"reasoning,omitempty"`
	ToolCalls   []ToolCall   `json:"tool_calls,omitempty"`
	ToolResults []ToolResult `json:"tool_results,omitempty"`
}

// Reasoning is model thinking returned with a reply. Providers that verify
// it (Anthropic, Bedrock) need it sent back unchanged with the turn it came
// from, so keep it in the history. It is only sent back to the provider that
// produced it.
type Reasoning struct {
	Provider  string `json:"provider"`
	Text      string `json:"text,omitempty"`
	Signature string `json:"signature,omitempty"`
	// Redacted is encrypted reasoning (Anthropic redacted_thinking data,
	// Bedrock redacted content, base64).
	Redacted string `json:"redacted,omitempty"`
}

// ToolCall is a tool invocation requested by the model.
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// Signature is Gemini's thought signature for this call; it must be sent
	// back with the call.
	Signature string `json:"signature,omitempty"`
}

// ToolResult is the outcome of one tool call, sent back in a user message.
type ToolResult struct {
	CallID  string `json:"call_id"`
	Name    string `json:"name"` // the tool name (Gemini matches results by name)
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

// Tool describes a function the model may call.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"` // JSON Schema of the input object
}

// ToolChoiceMode controls whether the model must call a tool.
type ToolChoiceMode string

const (
	ToolAuto     ToolChoiceMode = "auto"     // the model decides (default)
	ToolRequired ToolChoiceMode = "required" // the model must call some tool
	ToolNone     ToolChoiceMode = "none"     // the model must not call tools
	ToolSpecific ToolChoiceMode = "tool"     // the model must call ToolChoice.Name
)

// ToolChoice selects how tools are used. The zero value means auto.
type ToolChoice struct {
	Mode ToolChoiceMode
	Name string // for ToolSpecific
}

// Request is a single model call.
type Request struct {
	System      string
	Messages    []Message
	Tools       []Tool
	ToolChoice  ToolChoice
	MaxTokens   int      // 0 means the model config default
	Temperature *float64 // nil means the provider default
	// Cache asks providers with explicit prompt caching (Anthropic, Bedrock)
	// to cache the system prompt, tools and conversation so far. Others
	// cache automatically or not at all and ignore it.
	Cache bool
}

// StopReason is why the model stopped, normalised across providers.
type StopReason string

const (
	StopEnd       StopReason = "end"        // natural end of turn
	StopToolUse   StopReason = "tool_use"   // the reply asks for tool calls
	StopMaxTokens StopReason = "max_tokens" // output was cut off
	StopOther     StopReason = "other"      // content filter, refusal, unknown
)

// Usage reports token counts when the provider returns them.
type Usage struct {
	InputTokens  int `json:"input_tokens"` // all prompt tokens, including cache hits and writes
	OutputTokens int `json:"output_tokens"`
	// CachedInputTokens is the part of InputTokens read from a prompt cache.
	CachedInputTokens int `json:"cached_input_tokens,omitempty"`
	// CacheWriteTokens is the part of InputTokens written to a prompt cache.
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// Add returns the sum of two usages.
func (u Usage) Add(o Usage) Usage {
	return Usage{
		InputTokens:       u.InputTokens + o.InputTokens,
		OutputTokens:      u.OutputTokens + o.OutputTokens,
		CachedInputTokens: u.CachedInputTokens + o.CachedInputTokens,
		CacheWriteTokens:  u.CacheWriteTokens + o.CacheWriteTokens,
	}
}

// Response is a completed model call.
type Response struct {
	Message    Message // Role is always Assistant
	StopReason StopReason
	RawStop    string // the provider's own stop/finish reason
	Model      string
	Usage      Usage
}

// EventType identifies a streaming event.
type EventType string

const (
	EventText      EventType = "text"       // Text holds a text delta
	EventReasoning EventType = "reasoning"  // Text holds a reasoning delta
	EventToolStart EventType = "tool_start" // ToolCallID and ToolName are set
)

// Event is an incremental update while a reply streams in. The complete
// reply is always returned by Chat as well, so events are for display only.
type Event struct {
	Type       EventType
	Text       string
	ToolCallID string
	ToolName   string
}

// EventHandler receives streaming events. It is called from the goroutine
// that called Chat.
type EventHandler func(Event)

// Client is implemented by every provider.
type Client interface {
	// Name identifies the client in logs, e.g. "openai:gpt-x".
	Name() string
	// Chat sends one request. With a nil handler it uses the provider's
	// non-streaming API; otherwise it streams and calls handler with
	// deltas. Either way it returns the complete reply.
	Chat(ctx context.Context, req *Request, handler EventHandler) (*Response, error)
}

// APIError is an HTTP-level failure from a provider.
type APIError struct {
	Provider   string
	StatusCode int
	Body       string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	body := e.Body
	if len(body) > 500 {
		body = body[:500] + "…"
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.Provider, e.StatusCode, body)
}

// Retryable reports whether the request may succeed if sent again.
func (e *APIError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode == http.StatusRequestTimeout ||
		e.StatusCode == http.StatusConflict || e.StatusCode >= 500
}

// StreamError is an error the provider reported inside a stream after the
// HTTP response started (for example Anthropic's "overloaded_error").
type StreamError struct {
	Provider  string
	Type      string
	Message   string
	retryable bool
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("%s: stream error %s: %s", e.Provider, e.Type, e.Message)
}

// Retryable reports whether the request may succeed if sent again.
func (e *StreamError) Retryable() bool { return e.retryable }

// ErrNoJSON means the model did not return a parseable JSON object.
var ErrNoJSON = errors.New("model response contained no JSON object")

// IsRetryable reports whether err is worth retrying on the same provider.
func IsRetryable(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Retryable()
	}
	var re interface{ Retryable() bool }
	if errors.As(err, &re) {
		return re.Retryable()
	}
	// Network errors and timeouts (but not caller cancellation).
	if errors.Is(err, context.Canceled) {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) || isNetErr(err)
}

// Validate checks a request for mistakes that every provider would reject
// or mishandle, before anything is sent.
func (r *Request) Validate() error {
	if len(r.Messages) == 0 {
		return errors.New("request has no messages")
	}
	names := make(map[string]bool, len(r.Tools))
	for _, t := range r.Tools {
		if t.Name == "" {
			return errors.New("tool with empty name")
		}
		if names[t.Name] {
			return fmt.Errorf("duplicate tool %q", t.Name)
		}
		names[t.Name] = true
	}
	switch r.ToolChoice.Mode {
	case "", ToolAuto, ToolNone:
	case ToolRequired:
		if len(r.Tools) == 0 {
			return errors.New("tool choice \"required\" with no tools")
		}
	case ToolSpecific:
		if !names[r.ToolChoice.Name] {
			return fmt.Errorf("tool choice names unknown tool %q", r.ToolChoice.Name)
		}
	default:
		return fmt.Errorf("unknown tool choice mode %q", r.ToolChoice.Mode)
	}
	for i, m := range r.Messages {
		switch m.Role {
		case User:
			if len(m.ToolCalls) > 0 {
				return fmt.Errorf("message %d: user message with tool calls", i)
			}
		case Assistant:
			if len(m.ToolResults) > 0 {
				return fmt.Errorf("message %d: assistant message with tool results", i)
			}
		default:
			return fmt.Errorf("message %d: unknown role %q", i, m.Role)
		}
		for _, tc := range m.ToolCalls {
			if tc.ID == "" || tc.Name == "" {
				return fmt.Errorf("message %d: tool call without id or name", i)
			}
		}
		for _, tr := range m.ToolResults {
			if tr.CallID == "" {
				return fmt.Errorf("message %d: tool result without call id", i)
			}
		}
	}
	return nil
}
