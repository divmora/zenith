package llm

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"
)

func init() {
	Register("bedrock", func(cfg ModelConfig, _ *slog.Logger) (Client, error) { return newBedrock(cfg) })
}

const bedrockProvider = "bedrock"

// bedrockClient uses the Bedrock Converse API, which gives one request shape
// for every model family on Bedrock. Model may be a model ID, an inference
// profile ID (e.g. "us.<model-id>") or an ARN.
//
// Not every model supports every feature. Models that reject a forced tool
// choice, a system prompt or cache points are detected from the first
// ValidationException, remembered, and the request is sent again without
// that feature (a forced choice becomes auto; the system prompt moves into
// the first user message; caching is dropped).
type bedrockClient struct {
	cfg ModelConfig
	rt  *bedrockruntime.Client

	noForcedTool atomic.Bool
	noSystem     atomic.Bool
	noCache      atomic.Bool
}

func newBedrock(cfg ModelConfig) (*bedrockClient, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}
	if cfg.AWSProfile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(cfg.AWSProfile))
	}
	// Retries are handled by WithRetry so behaviour matches other providers.
	opts = append(opts, awsconfig.WithRetryMaxAttempts(1))
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	if awsCfg.Region == "" {
		return nil, errors.New("AWS region is not set: use region in the model config or AWS_REGION")
	}
	rt := bedrockruntime.NewFromConfig(awsCfg, func(o *bedrockruntime.Options) {
		if cfg.BaseURL != "" {
			o.BaseEndpoint = aws.String(cfg.BaseURL)
		}
	})
	return &bedrockClient{cfg: cfg, rt: rt}, nil
}

func (c *bedrockClient) Name() string { return "bedrock:" + c.cfg.Model }

// brShape is what a request may use, after downgrades.
type brShape struct {
	forced, systemInUser, cache bool
}

func (c *bedrockClient) Chat(ctx context.Context, req *Request, handler EventHandler) (*Response, error) {
	ctx, cancel := withTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	wantsForced := req.ToolChoice.Mode == ToolRequired || req.ToolChoice.Mode == ToolSpecific
	for attempt := 0; attempt < 4; attempt++ {
		shape := brShape{
			forced:       wantsForced && !c.noForcedTool.Load(),
			systemInUser: c.noSystem.Load(),
			cache:        req.Cache && !c.noCache.Load(),
		}
		in := c.input(req, shape)
		var resp *Response
		var err error
		if handler == nil {
			resp, err = c.converse(ctx, in)
		} else {
			resp, err = c.converseStream(ctx, in, handler)
		}
		if err == nil {
			return resp, nil
		}
		msg := strings.ToLower(validationMessage(err))
		switch {
		case shape.cache && isCacheDenied(err):
			// Models without prompt caching answer cache points with
			// AccessDeniedException, not ValidationException.
			c.noCache.Store(true)
		case msg == "":
			return nil, err
		case shape.cache && strings.Contains(msg, "cach"):
			c.noCache.Store(true)
		case shape.forced && strings.Contains(msg, "tool"):
			c.noForcedTool.Store(true)
		case !shape.systemInUser && req.System != "" && strings.Contains(msg, "system"):
			c.noSystem.Store(true)
		default:
			return nil, err
		}
	}
	return nil, fmt.Errorf("%s: model rejected every request shape", c.Name())
}

// brInput holds the fields shared by Converse and ConverseStream.
type brInput struct {
	messages []types.Message
	system   []types.SystemContentBlock
	inf      *types.InferenceConfiguration
	tools    *types.ToolConfiguration
}

func (c *bedrockClient) input(req *Request, shape brShape) brInput {
	var in brInput
	if req.System != "" && !shape.systemInUser {
		in.system = []types.SystemContentBlock{&types.SystemContentBlockMemberText{Value: req.System}}
		if shape.cache {
			in.system = append(in.system, &types.SystemContentBlockMemberCachePoint{Value: types.CachePointBlock{Type: types.CachePointTypeDefault}})
		}
	}
	for i, m := range req.Messages {
		var blocks []types.ContentBlock
		role := types.ConversationRoleUser
		switch m.Role {
		case User:
			for _, tr := range m.ToolResults {
				status := types.ToolResultStatusSuccess
				if tr.IsError {
					status = types.ToolResultStatusError
				}
				content := tr.Content
				if content == "" {
					content = "(no output)" // Bedrock rejects blank text
				}
				blocks = append(blocks, &types.ContentBlockMemberToolResult{Value: types.ToolResultBlock{
					ToolUseId: aws.String(tr.CallID),
					Content:   []types.ToolResultContentBlock{&types.ToolResultContentBlockMemberText{Value: content}},
					Status:    status,
				}})
			}
			text := m.Text
			if i == 0 && shape.systemInUser && req.System != "" {
				text = "<instructions>\n" + req.System + "\n</instructions>\n\n" + text
			}
			if text != "" || len(blocks) == 0 {
				blocks = append(blocks, &types.ContentBlockMemberText{Value: firstNonEmpty(text, " ")})
			}
		case Assistant:
			role = types.ConversationRoleAssistant
			for _, r := range m.Reasoning {
				if r.Provider != bedrockProvider {
					continue
				}
				if r.Redacted != "" {
					raw, err := base64.StdEncoding.DecodeString(r.Redacted)
					if err == nil {
						blocks = append(blocks, &types.ContentBlockMemberReasoningContent{Value: &types.ReasoningContentBlockMemberRedactedContent{Value: raw}})
					}
					continue
				}
				rt := types.ReasoningTextBlock{Text: aws.String(r.Text)}
				if r.Signature != "" {
					rt.Signature = aws.String(r.Signature)
				}
				blocks = append(blocks, &types.ContentBlockMemberReasoningContent{Value: &types.ReasoningContentBlockMemberReasoningText{Value: rt}})
			}
			if m.Text != "" {
				blocks = append(blocks, &types.ContentBlockMemberText{Value: m.Text})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, &types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{
					ToolUseId: aws.String(tc.ID),
					Name:      aws.String(tc.Name),
					Input:     document.NewLazyDocument(inputObject(tc.Input)),
				}})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, &types.ContentBlockMemberText{Value: " "})
			}
		}
		in.messages = append(in.messages, types.Message{Role: role, Content: blocks})
	}
	if shape.cache && len(in.messages) > 0 {
		last := &in.messages[len(in.messages)-1]
		last.Content = append(last.Content, &types.ContentBlockMemberCachePoint{Value: types.CachePointBlock{Type: types.CachePointTypeDefault}})
	}

	in.inf = &types.InferenceConfiguration{MaxTokens: aws.Int32(int32(maxTokens(req, c.cfg)))}
	if t := firstTemp(req.Temperature, c.cfg.Temperature); t != nil {
		in.inf.Temperature = aws.Float32(float32(*t))
	}

	// Bedrock needs the tool config whenever the history has tool blocks,
	// and has no "none" choice: with ToolNone the tools stay on auto.
	if len(req.Tools) > 0 {
		tc := &types.ToolConfiguration{}
		for _, t := range req.Tools {
			tc.Tools = append(tc.Tools, &types.ToolMemberToolSpec{Value: types.ToolSpecification{
				Name:        aws.String(t.Name),
				Description: aws.String(firstNonEmpty(t.Description, t.Name)),
				InputSchema: &types.ToolInputSchemaMemberJson{Value: document.NewLazyDocument(schemaOrEmpty(t.InputSchema))},
			}})
		}
		if shape.cache {
			tc.Tools = append(tc.Tools, &types.ToolMemberCachePoint{Value: types.CachePointBlock{Type: types.CachePointTypeDefault}})
		}
		if shape.forced {
			switch req.ToolChoice.Mode {
			case ToolRequired:
				tc.ToolChoice = &types.ToolChoiceMemberAny{Value: types.AnyToolChoice{}}
			case ToolSpecific:
				tc.ToolChoice = &types.ToolChoiceMemberTool{Value: types.SpecificToolChoice{Name: aws.String(req.ToolChoice.Name)}}
			}
		}
		in.tools = tc
	}
	return in
}

func (c *bedrockClient) converse(ctx context.Context, in brInput) (*Response, error) {
	out, err := c.rt.Converse(ctx, &bedrockruntime.ConverseInput{
		ModelId:         aws.String(c.cfg.Model),
		Messages:        in.messages,
		System:          in.system,
		InferenceConfig: in.inf,
		ToolConfig:      in.tools,
	})
	if err != nil {
		return nil, wrapAWSErr(err)
	}
	m, ok := out.Output.(*types.ConverseOutputMemberMessage)
	if !ok {
		return nil, fmt.Errorf("bedrock: unexpected output type %T", out.Output)
	}
	msg := Message{Role: Assistant}
	var text strings.Builder
	for _, b := range m.Value.Content {
		switch v := b.(type) {
		case *types.ContentBlockMemberText:
			text.WriteString(v.Value)
		case *types.ContentBlockMemberToolUse:
			var raw []byte
			if v.Value.Input != nil {
				// MarshalSmithyDocument re-encodes the original JSON.
				// Decoding into `any` first would turn numbers into strings.
				if raw, err = v.Value.Input.MarshalSmithyDocument(); err != nil {
					return nil, fmt.Errorf("bedrock: encode tool input: %w", err)
				}
			}
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:    firstNonEmpty(aws.ToString(v.Value.ToolUseId), newCallID()),
				Name:  aws.ToString(v.Value.Name),
				Input: toolInput(raw),
			})
		case *types.ContentBlockMemberReasoningContent:
			switch r := v.Value.(type) {
			case *types.ReasoningContentBlockMemberReasoningText:
				msg.Reasoning = append(msg.Reasoning, Reasoning{Provider: bedrockProvider, Text: aws.ToString(r.Value.Text), Signature: aws.ToString(r.Value.Signature)})
			case *types.ReasoningContentBlockMemberRedactedContent:
				msg.Reasoning = append(msg.Reasoning, Reasoning{Provider: bedrockProvider, Redacted: base64.StdEncoding.EncodeToString(r.Value)})
			}
		}
	}
	msg.Text = text.String()
	return &Response{
		Message:    msg,
		StopReason: brStop(string(out.StopReason), len(msg.ToolCalls) > 0),
		RawStop:    string(out.StopReason),
		Model:      c.cfg.Model,
		Usage:      brUsage(out.Usage),
	}, nil
}

type brPartial struct {
	kind      string // text, tool, reasoning
	id, name  string
	text      strings.Builder
	signature string
	redacted  []byte
}

func (c *bedrockClient) converseStream(ctx context.Context, in brInput, handler EventHandler) (*Response, error) {
	out, err := c.rt.ConverseStream(ctx, &bedrockruntime.ConverseStreamInput{
		ModelId:         aws.String(c.cfg.Model),
		Messages:        in.messages,
		System:          in.system,
		InferenceConfig: in.inf,
		ToolConfig:      in.tools,
	})
	if err != nil {
		return nil, wrapAWSErr(err)
	}
	stream := out.GetStream()
	defer func() { _ = stream.Close() }()

	var (
		order   []int
		blocks  = map[int]*brPartial{}
		stop    string
		usage   Usage
		partial = func(idx int, kind string) *brPartial {
			p := blocks[idx]
			if p == nil {
				p = &brPartial{kind: kind}
				blocks[idx] = p
				order = append(order, idx)
			}
			return p
		}
	)
	for ev := range stream.Events() {
		switch v := ev.(type) {
		case *types.ConverseStreamOutputMemberContentBlockStart:
			idx := int(aws.ToInt32(v.Value.ContentBlockIndex))
			if s, ok := v.Value.Start.(*types.ContentBlockStartMemberToolUse); ok {
				p := partial(idx, "tool")
				p.id = aws.ToString(s.Value.ToolUseId)
				p.name = aws.ToString(s.Value.Name)
				handler(Event{Type: EventToolStart, ToolCallID: p.id, ToolName: p.name})
			}
		case *types.ConverseStreamOutputMemberContentBlockDelta:
			idx := int(aws.ToInt32(v.Value.ContentBlockIndex))
			switch d := v.Value.Delta.(type) {
			case *types.ContentBlockDeltaMemberText:
				partial(idx, "text").text.WriteString(d.Value)
				handler(Event{Type: EventText, Text: d.Value})
			case *types.ContentBlockDeltaMemberToolUse:
				partial(idx, "tool").text.WriteString(aws.ToString(d.Value.Input))
			case *types.ContentBlockDeltaMemberReasoningContent:
				p := partial(idx, "reasoning")
				switch r := d.Value.(type) {
				case *types.ReasoningContentBlockDeltaMemberText:
					p.text.WriteString(r.Value)
					handler(Event{Type: EventReasoning, Text: r.Value})
				case *types.ReasoningContentBlockDeltaMemberSignature:
					p.signature += r.Value
				case *types.ReasoningContentBlockDeltaMemberRedactedContent:
					p.redacted = append(p.redacted, r.Value...)
				}
			}
		case *types.ConverseStreamOutputMemberMessageStop:
			stop = string(v.Value.StopReason)
		case *types.ConverseStreamOutputMemberMetadata:
			usage = brUsage(v.Value.Usage)
		}
	}
	if err := stream.Err(); err != nil {
		return nil, wrapAWSErr(err)
	}

	msg := Message{Role: Assistant}
	var text strings.Builder
	for _, idx := range order {
		p := blocks[idx]
		switch p.kind {
		case "text":
			text.WriteString(p.text.String())
		case "tool":
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:    firstNonEmpty(p.id, newCallID()),
				Name:  p.name,
				Input: toolInput([]byte(p.text.String())),
			})
		case "reasoning":
			r := Reasoning{Provider: bedrockProvider, Text: p.text.String(), Signature: p.signature}
			if len(p.redacted) > 0 {
				r = Reasoning{Provider: bedrockProvider, Redacted: base64.StdEncoding.EncodeToString(p.redacted)}
			}
			msg.Reasoning = append(msg.Reasoning, r)
		}
	}
	msg.Text = text.String()
	return &Response{
		Message:    msg,
		StopReason: brStop(stop, len(msg.ToolCalls) > 0),
		RawStop:    stop,
		Model:      c.cfg.Model,
		Usage:      usage,
	}, nil
}

// brUsage counts all input: some models report prompt-cache reads and
// writes separately from InputTokens.
func brUsage(u *types.TokenUsage) Usage {
	if u == nil {
		return Usage{}
	}
	read := int(aws.ToInt32(u.CacheReadInputTokens))
	write := int(aws.ToInt32(u.CacheWriteInputTokens))
	return Usage{
		InputTokens:       int(aws.ToInt32(u.InputTokens)) + read + write,
		OutputTokens:      int(aws.ToInt32(u.OutputTokens)),
		CachedInputTokens: read,
		CacheWriteTokens:  write,
	}
}

func brStop(reason string, hasCalls bool) StopReason {
	switch {
	case hasCalls || reason == "tool_use":
		return StopToolUse
	case reason == "end_turn" || reason == "stop_sequence" || reason == "":
		return StopEnd
	case reason == "max_tokens" || reason == "model_context_window_exceeded":
		return StopMaxTokens
	default:
		return StopOther // guardrail_intervened, content_filtered, ...
	}
}

// validationMessage returns the message of a Bedrock ValidationException,
// or "" for any other error.
func validationMessage(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) && api.ErrorCode() == "ValidationException" {
		return api.ErrorMessage()
	}
	return ""
}

// isCacheDenied reports Bedrock's AccessDeniedException for a request with
// cache points sent to a model without prompt caching ("You invoked an
// unsupported model or your request did not allow prompt caching").
func isCacheDenied(err error) bool {
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == "AccessDeniedException" &&
		strings.Contains(strings.ToLower(api.ErrorMessage()), "prompt caching")
}

// awsErr marks throttling and server faults as retryable.
type awsErr struct{ error }

func (e awsErr) Unwrap() error { return e.error }

func (e awsErr) Retryable() bool {
	var api smithy.APIError
	if errors.As(e.error, &api) {
		switch api.ErrorCode() {
		case "ThrottlingException", "ServiceUnavailableException", "InternalServerException",
			"ModelNotReadyException", "ModelTimeoutException", "ModelStreamErrorException":
			return true
		}
		return api.ErrorFault() == smithy.FaultServer
	}
	return isNetErr(e.error)
}

func wrapAWSErr(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	return awsErr{fmt.Errorf("bedrock: %w", err)}
}
