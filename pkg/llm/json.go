package llm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Schema describes a JSON object the model should return.
type Schema struct {
	Name        string
	Description string
	JSON        map[string]any // JSON Schema of the object
}

// CompleteJSON asks for a JSON object matching schema. It defines a single
// tool with the schema as its input and forces the model to call it, which
// every provider supports; if a model answers in text instead, the first
// JSON object in the text is used.
//
// req.Tools and req.ToolChoice are replaced; req is not modified.
func CompleteJSON(ctx context.Context, c Client, req *Request, schema Schema) (json.RawMessage, *Response, error) {
	name := firstNonEmpty(schema.Name, "respond")
	r := *req
	r.Tools = []Tool{{
		Name:        name,
		Description: firstNonEmpty(schema.Description, "Return the result."),
		InputSchema: schema.JSON,
	}}
	r.ToolChoice = ToolChoice{Mode: ToolSpecific, Name: name}
	resp, err := c.Chat(ctx, &r, nil)
	if err != nil {
		return nil, nil, err
	}
	for _, tc := range resp.Message.ToolCalls {
		if tc.Name == name && len(tc.Input) > 0 {
			return tc.Input, resp, nil
		}
	}
	raw, err := ExtractJSON(resp.Message.Text)
	if err != nil {
		return nil, resp, fmt.Errorf("%s (stop=%s): %w", c.Name(), resp.RawStop, err)
	}
	return raw, resp, nil
}

// ExtractJSON finds the first JSON object in model text. It accepts bare
// JSON, ```json fenced blocks, and JSON surrounded by prose.
func ExtractJSON(text string) (json.RawMessage, error) {
	t := strings.TrimSpace(text)
	if i := strings.Index(t, "```"); i >= 0 {
		rest := t[i+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if j := strings.Index(rest, "```"); j >= 0 {
			if raw, err := firstObject(rest[:j]); err == nil {
				return raw, nil
			}
		}
	}
	return firstObject(t)
}

func firstObject(s string) (json.RawMessage, error) {
	for start := strings.IndexByte(s, '{'); start >= 0; {
		dec := json.NewDecoder(strings.NewReader(s[start:]))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == nil {
			return raw, nil
		}
		next := strings.IndexByte(s[start+1:], '{')
		if next < 0 {
			break
		}
		start += next + 1
	}
	return nil, ErrNoJSON
}

// toolInput normalises tool-call arguments from a provider into a JSON
// object. Empty input becomes {}. Input that is not valid JSON (a model
// can emit broken arguments) is kept as a JSON string under "_raw" so the
// caller can report it back to the model instead of failing the turn.
func toolInput(raw []byte) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	if json.Valid(raw) {
		return json.RawMessage(raw)
	}
	b, _ := json.Marshal(map[string]string{"_raw": string(raw)})
	return b
}

// inputObject decodes tool-call input into a map for providers whose
// request format needs an object rather than raw JSON.
func inputObject(in json.RawMessage) map[string]any {
	out := map[string]any{}
	if len(in) == 0 {
		return out
	}
	if err := json.Unmarshal(in, &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

// schemaOrEmpty returns a tool input schema, defaulting to an empty object.
func schemaOrEmpty(s map[string]any) map[string]any {
	if s == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return s
}

// newCallID makes a tool-call id for providers that don't send one. Ids
// must stay unique across the conversation, because a fallback provider may
// receive the whole history.
func newCallID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "call_" + hex.EncodeToString(b[:])
}
