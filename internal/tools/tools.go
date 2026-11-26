// Package tools implements the tools the agent's model can call. Every tool
// runs under internal/policy: file paths stay inside the workspace, .git is
// read-only, and commands get a scrubbed environment.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/divmora/zenith/internal/policy"
	"github.com/divmora/zenith/pkg/llm"
)

// Finish is the name of the tool the model calls when it is done.
const Finish = "finish"

// Env is what tools run against.
type Env struct {
	WS         *policy.Workspace
	CommandEnv []string // environment for run_command (see policy.CommandEnv)
	// MaxOutput caps what one tool result sends back to the model.
	MaxOutput int
	// CommandTimeout is run_command's default; MaxCommandTimeout caps what
	// the model may ask for.
	CommandTimeout    time.Duration
	MaxCommandTimeout time.Duration
}

// DefaultEnv returns an Env with standard limits.
func DefaultEnv(ws *policy.Workspace, commandEnv []string) *Env {
	return &Env{
		WS:                ws,
		CommandEnv:        commandEnv,
		MaxOutput:         30_000,
		CommandTimeout:    2 * time.Minute,
		MaxCommandTimeout: 10 * time.Minute,
	}
}

// Tool is one callable tool.
type Tool struct {
	Def      llm.Tool
	ReadOnly bool // never changes the workspace
	Run      func(ctx context.Context, env *Env, input json.RawMessage) (string, error)
}

// Set is the tools offered to the model.
type Set struct {
	tools  []Tool
	byName map[string]Tool
}

// NewSet builds a set; names must be unique.
func NewSet(ts ...Tool) *Set {
	s := &Set{byName: map[string]Tool{}}
	for _, t := range ts {
		if _, dup := s.byName[t.Def.Name]; dup {
			panic("tools: duplicate tool " + t.Def.Name)
		}
		s.tools = append(s.tools, t)
		s.byName[t.Def.Name] = t
	}
	return s
}

// Default returns every built-in tool.
func Default() *Set {
	return NewSet(ReadFile(), ListFiles(), Grep(), WriteFile(), EditFile(), RunCommand(), FinishTool())
}

// Defs returns the tool definitions for a model request.
func (s *Set) Defs() []llm.Tool {
	out := make([]llm.Tool, len(s.tools))
	for i, t := range s.tools {
		out[i] = t.Def
	}
	return out
}

// Get returns a tool by name.
func (s *Set) Get(name string) (Tool, bool) {
	t, ok := s.byName[name]
	return t, ok
}

// Call runs one tool call and returns the text for the model. Failures are
// returned as text with isError set, so the model can correct itself; they
// never abort the run.
func (s *Set) Call(ctx context.Context, env *Env, call llm.ToolCall) (output string, isError bool) {
	t, ok := s.byName[call.Name]
	if !ok {
		names := make([]string, 0, len(s.byName))
		for n := range s.byName {
			names = append(names, n)
		}
		sort.Strings(names)
		return fmt.Sprintf("unknown tool %q; available tools: %s", call.Name, strings.Join(names, ", ")), true
	}
	out, err := t.Run(ctx, env, call.Input)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			if ctx.Err() != nil {
				return "the run was stopped: " + ctx.Err().Error(), true
			}
		}
		return policy.Truncate(err.Error(), env.MaxOutput), true
	}
	return policy.Truncate(out, env.MaxOutput), false
}

// decode parses tool input into v, with an error the model can act on.
func decode(input json.RawMessage, v any) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(input, &probe); err == nil {
		if raw, ok := probe["_raw"]; ok && len(probe) == 1 {
			return fmt.Errorf("the tool arguments were not valid JSON: %s", raw)
		}
	}
	if err := json.Unmarshal(input, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// schema builds a JSON Schema object from properties; required lists the
// mandatory ones.
func schema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any  { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any  { return map[string]any{"type": "integer", "description": desc} }
func flag(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }

// FinishTool ends the task. The harness reads the summary.
func FinishTool() Tool {
	return Tool{
		Def: llm.Tool{
			Name: Finish,
			Description: "Call this exactly once when the task is complete (or cannot be completed). " +
				"Give a short summary for the pull request: what you changed, why, and how you verified it. " +
				"If you could not complete the task, say so and set success to false.",
			InputSchema: schema(map[string]any{
				"summary": str("Summary of the change for the pull request description (Markdown)."),
				"success": flag("true if the task is complete; false if it could not be done."),
			}, "summary", "success"),
		},
		ReadOnly: true,
		Run: func(_ context.Context, _ *Env, input json.RawMessage) (string, error) {
			var in FinishInput
			if err := decode(input, &in); err != nil {
				return "", err
			}
			if strings.TrimSpace(in.Summary) == "" {
				return "", errors.New("summary is required")
			}
			return "Task finished.", nil
		},
	}
}

// FinishInput is the finish tool's input.
type FinishInput struct {
	Summary string `json:"summary"`
	Success bool   `json:"success"`
}

// ProposePlan is the name of the tool that ends the planning phase.
const ProposePlan = "propose_plan"

// ProposePlanTool submits a plan for approval. The harness reads the plan.
func ProposePlanTool() Tool {
	return Tool{
		Def: llm.Tool{
			Name: ProposePlan,
			Description: "Submit your implementation plan for human review. Call this once you understand the code well enough. " +
				"The plan should list the files you will change or add and what changes, how you will verify it (which tests or commands), " +
				"and any assumptions or open questions. Nothing is changed until the plan is approved.",
			InputSchema: schema(map[string]any{
				"plan": str("The plan in Markdown."),
			}, "plan"),
		},
		ReadOnly: true,
		Run: func(_ context.Context, _ *Env, input json.RawMessage) (string, error) {
			var in PlanInput
			if err := decode(input, &in); err != nil {
				return "", err
			}
			if strings.TrimSpace(in.Plan) == "" {
				return "", errors.New("plan is required")
			}
			return "Plan submitted for review.", nil
		},
	}
}

// PlanInput is the propose_plan tool's input.
type PlanInput struct {
	Plan string `json:"plan"`
}

// Planning returns the tools for the planning phase: read-only exploration
// plus propose_plan. Nothing here can change the workspace.
func Planning() *Set {
	return NewSet(ReadFile(), ListFiles(), Grep(), ProposePlanTool())
}
