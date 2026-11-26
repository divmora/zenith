// Package protocol is the contract between the zenith CLI and the programs
// that run it (Zenith Console, CI jobs, scripts): the event stream, the
// result file and the exit codes. Keep it backward compatible: add fields,
// never rename or repurpose them, and bump Version only for breaking changes.
package protocol

import (
	"encoding/json"
	"time"

	"github.com/divmora/zenith/pkg/llm"
)

// Version of the event and result format.
const Version = 1

// Exit codes of `zenith run` (and later `resume`).
const (
	ExitDone             = 0  // the task finished
	ExitInternal         = 1  // zenith itself failed (bad setup, I/O error, bug)
	ExitUsage            = 2  // invalid flags or arguments
	ExitAwaitingApproval = 10 // a plan is waiting for approval (M3)
	ExitBudgetExceeded   = 20 // a step, token or time limit was reached
	ExitFailed           = 30 // the agent could not complete the task
)

// Status is the final state of a task.
type Status string

const (
	StatusDone             Status = "done"
	StatusFailed           Status = "failed"
	StatusBudgetExceeded   Status = "budget_exceeded"
	StatusAwaitingApproval Status = "awaiting_approval"
)

// ExitCode maps a status to the process exit code.
func (s Status) ExitCode() int {
	switch s {
	case StatusDone:
		return ExitDone
	case StatusAwaitingApproval:
		return ExitAwaitingApproval
	case StatusBudgetExceeded:
		return ExitBudgetExceeded
	case StatusFailed:
		return ExitFailed
	default:
		return ExitInternal
	}
}

// EventType names an event in the stream.
type EventType string

const (
	EventTaskStarted    EventType = "task_started"
	EventAssistantText  EventType = "assistant_text" // one complete assistant message text
	EventToolCall       EventType = "tool_call"
	EventToolResult     EventType = "tool_result"
	EventUsage          EventType = "usage" // after every model call; totals so far
	EventCommitted      EventType = "committed"
	EventTaskFinished   EventType = "task_finished" // always the last event; carries the Result
	EventWarning        EventType = "warning"
	EventModelCallStart EventType = "model_call"    // a model call is starting (step number)
	EventPlanProposed   EventType = "plan_proposed" // Text holds the plan; the run then stops for approval
	EventResumed        EventType = "resumed"       // a run continued from a checkpoint; Text says how
	EventCheck          EventType = "check"         // zenith ran a verification check; Check holds it
	EventCompacted      EventType = "compacted"     // the conversation was shortened; Text says how
)

// Phase is the part of a run the agent is in.
type Phase string

const (
	PhasePlan    Phase = "plan"    // read-only exploration, ends with a proposed plan
	PhaseExecute Phase = "execute" // edits, commands, ends with finish
)

// Event is one line of the JSON event stream.
type Event struct {
	V    int       `json:"v"`
	Seq  int       `json:"seq"`
	Time time.Time `json:"time"`
	Type EventType `json:"type"`

	Step  int    `json:"step,omitempty"`
	Phase Phase  `json:"phase,omitempty"`
	Text  string `json:"text,omitempty"`
	Model string `json:"model,omitempty"`

	Tool   *ToolEvent `json:"tool,omitempty"`
	Usage  *Usage     `json:"usage,omitempty"`
	Task   *TaskInfo  `json:"task,omitempty"`
	Commit *Commit    `json:"commit,omitempty"`
	Check  *Check     `json:"check,omitempty"`
	Result *Result    `json:"result,omitempty"`
}

// CheckStage says when a check ran.
type CheckStage string

const (
	CheckBaseline CheckStage = "baseline" // before any change, to find failures that were already there
	CheckFinal    CheckStage = "final"    // after the agent finished (possibly several rounds)
)

// Check is one verification command zenith ran itself (not the model).
type Check struct {
	Command    string     `json:"command"`
	Stage      CheckStage `json:"stage"`
	Round      int        `json:"round,omitempty"` // 1 for the first final check, 2 after one fix attempt, ...
	ExitCode   int        `json:"exit_code"`
	Passed     bool       `json:"passed"`
	TimedOut   bool       `json:"timed_out,omitempty"`
	DurationMS int64      `json:"duration_ms"`
	Output     string     `json:"output,omitempty"` // truncated
	// PreExisting is set on a failing final check that also failed at the
	// baseline: it is reported but does not fail the run.
	PreExisting bool `json:"pre_existing,omitempty"`
}

// ToolEvent describes a tool call or its result.
type ToolEvent struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Input      json.RawMessage `json:"input,omitempty"`  // tool_call
	Output     string          `json:"output,omitempty"` // tool_result (possibly truncated)
	IsError    bool            `json:"is_error,omitempty"`
	DurationMS int64           `json:"duration_ms,omitempty"`
}

// Usage is token usage summed over all model calls so far.
type Usage struct {
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	CachedInputTokens int `json:"cached_input_tokens,omitempty"`
	CacheWriteTokens  int `json:"cache_write_tokens,omitempty"`
	ModelCalls        int `json:"model_calls"`
}

// Total is input plus output tokens.
func (u Usage) Total() int { return u.InputTokens + u.OutputTokens }

// TaskInfo is sent with task_started.
type TaskInfo struct {
	Task      string `json:"task"`
	Workspace string `json:"workspace"`
	Model     string `json:"model"`
	BaseSHA   string `json:"base_sha,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Worktree  string `json:"worktree,omitempty"` // set when the run works in its own git worktree
	Persona   string `json:"persona,omitempty"`
}

// Commit describes the commit the harness made.
type Commit struct {
	SHA     string   `json:"sha"`
	Branch  string   `json:"branch"`
	Message string   `json:"message"`
	Files   []string `json:"files"`
}

// Limits are the budgets a run was given.
type Limits struct {
	MaxSteps       int   `json:"max_steps"`
	MaxTokens      int   `json:"max_tokens"`
	TimeoutS       int64 `json:"timeout_s"`
	MaxFixAttempts int   `json:"max_fix_attempts,omitempty"`
	ContextTokens  int   `json:"context_tokens,omitempty"`
}

// Result is the outcome of a run. It is written to the result file and sent
// with the task_finished event.
type Result struct {
	V           int       `json:"v"`
	Status      Status    `json:"status"`
	Summary     string    `json:"summary,omitempty"`    // the agent's own summary of what it did
	Plan        string    `json:"plan,omitempty"`       // the proposed (or approved) plan
	Checkpoint  string    `json:"checkpoint,omitempty"` // checkpoint file written for awaiting_approval
	Error       string    `json:"error,omitempty"`      // why it failed or stopped
	Task        string    `json:"task"`
	Persona     string    `json:"persona,omitempty"`
	Model       string    `json:"model"`
	Workspace   string    `json:"workspace,omitempty"` // the repository zenith was pointed at
	Branch      string    `json:"branch,omitempty"`
	BaseBranch  string    `json:"base_branch,omitempty"` // the branch the work started from, when HEAD was on one
	Worktree    string    `json:"worktree,omitempty"`    // the git worktree holding Branch, when one was used
	BaseSHA     string    `json:"base_sha,omitempty"`
	CommitSHA   string    `json:"commit_sha,omitempty"`  // empty when nothing changed
	Files       []string  `json:"files,omitempty"`       // files changed relative to BaseSHA
	Checks      []Check   `json:"checks,omitempty"`      // verification zenith ran (baseline, then the last final round)
	Commands    []string  `json:"commands,omitempty"`    // shell commands the agent ran, in order (capped)
	Compactions int       `json:"compactions,omitempty"` // times the conversation was shortened
	Steps       int       `json:"steps"`                 // model calls in this invocation
	Usage       Usage     `json:"usage"`                 // tokens over the whole task, including earlier invocations
	Limits      Limits    `json:"limits"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
}

// Persona is the specialist role a run uses (see `zenith personas`).
type Persona struct {
	Name         string `json:"name"`
	Emoji        string `json:"emoji,omitempty"`
	Title        string `json:"title,omitempty"`
	Instructions string `json:"instructions"`
}

// CheckpointVersion is the checkpoint file format version.
const CheckpointVersion = 1

// Checkpoint is what `zenith run --approve-plan` saves when it stops for
// approval, and what `zenith resume` continues from, possibly in a fresh
// sandbox. Planning only reads, so the repository at BaseSHA plus the
// conversation is the whole state.
//
// The conversation contains repository content: store checkpoints like
// source code.
type Checkpoint struct {
	V          int           `json:"v"`
	Task       string        `json:"task"`
	Model      string        `json:"model"` // provider:model spec of the planning model
	BaseSHA    string        `json:"base_sha"`
	Branch     string        `json:"branch"`                // branch the execution phase will create
	BaseBranch string        `json:"base_branch,omitempty"` // branch the plan started from, if any
	Plan       string        `json:"plan"`
	Persona    *Persona      `json:"persona,omitempty"`
	Messages   []llm.Message `json:"messages"`
	Usage      Usage         `json:"usage"`  // over the whole task so far
	Rounds     int           `json:"rounds"` // plans proposed so far (1 + feedback rounds)
	CreatedAt  time.Time     `json:"created_at"`
}
