// Package harness runs one task: the agent loop (model → tool calls →
// results → model ...), its budgets, the optional plan-approval stop, and
// the git branch and commit around it.
//
// A run is one of:
//   - execute: explore, edit, test, finish, commit on a new branch;
//   - plan first: explore read-only, propose a plan, save a checkpoint and
//     stop with awaiting_approval;
//   - resume with approval: continue the checkpoint in the execute phase;
//   - resume with feedback: continue planning and propose a revised plan.
package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/divmora/zenith/internal/gitx"
	"github.com/divmora/zenith/internal/policy"
	"github.com/divmora/zenith/internal/prompt"
	"github.com/divmora/zenith/internal/tools"
	"github.com/divmora/zenith/internal/verify"
	"github.com/divmora/zenith/pkg/llm"
	"github.com/divmora/zenith/pkg/protocol"
)

// Config is everything one run needs.
type Config struct {
	Task string // ignored when resuming: the checkpoint has it
	// Persona is the specialist role, if any (ignored when resuming: the
	// checkpoint has it).
	Persona *protocol.Persona
	Model   llm.Client
	// ModelSpec is the provider:model spec saved in checkpoints.
	ModelSpec string
	Tools     *tools.Set // execute phase; nil = tools.Default()
	PlanTools *tools.Set // plan phase; nil = tools.Planning()
	ToolEnv   *tools.Env
	MaxSteps  int           // model calls in this invocation; 0 = 100
	MaxTokens int           // tokens over the whole task; 0 = 5,000,000
	Timeout   time.Duration // this invocation; 0 = 1 hour
	// Branch to create for the work; "" = zenith/<slug>-<random>.
	Branch string
	// AllowDirty lets a run start with uncommitted changes, which then end
	// up in zenith's commit.
	AllowDirty bool
	// CommitName and CommitEmail set the author and committer.
	CommitName, CommitEmail string

	// Verify lists the check commands zenith runs itself after the agent
	// finishes; nil detects them from the project (see verify.Detect).
	// NoVerify turns verification off.
	Verify   []string
	NoVerify bool
	// MaxFixAttempts is how many times the agent gets the failing output
	// back to fix it; 0 = 3, negative = none.
	MaxFixAttempts int
	// CheckTimeout bounds each check command; 0 = 10 minutes.
	CheckTimeout time.Duration
	// ContextTokens is the model's context window; the conversation is
	// compacted before it would pass 75% of it. 0 = 128,000.
	ContextTokens int

	// Worktree runs the task in a new git worktree instead of the
	// workspace's own checkout, which is then never touched: its branch,
	// HEAD and uncommitted changes stay as they are. The work starts from
	// the checkout's HEAD commit (or the checkpoint's base commit).
	Worktree bool
	// WorktreeDir is where to create it; "" = DefaultWorktreeDir.
	WorktreeDir string

	// PlanFirst stops after a plan is proposed (new runs only).
	PlanFirst bool
	// CheckpointPath is where a proposed plan's checkpoint is written.
	// Required for PlanFirst and for resuming with Feedback.
	CheckpointPath string
	// Resume continues from a checkpoint, with either Approve or Feedback.
	Resume   *protocol.Checkpoint
	Approve  bool
	Feedback string
	// ApproveNotes are reviewer notes sent with an approval.
	ApproveNotes string

	// Emit receives every event (in order, from one goroutine). Optional.
	Emit func(protocol.Event)
	// OnDelta receives streaming text while the model replies. When nil,
	// non-streaming calls are used. Optional.
	OnDelta llm.EventHandler
	Logger  *slog.Logger
}

// SetupError means the run could not start (not a repo, dirty tree, wrong
// commit for a checkpoint, ...). No model call was made.
type SetupError struct{ Err error }

func (e *SetupError) Error() string { return e.Err.Error() }
func (e *SetupError) Unwrap() error { return e.Err }

const (
	defaultSteps   = 100
	defaultTokens  = 5_000_000
	defaultTimeout = time.Hour
	maxNudges      = 2

	defaultFixAttempts   = 3
	defaultCheckTimeout  = 10 * time.Minute
	defaultContextTokens = 128_000
)

type runner struct {
	cfg    Config
	git    gitx.Git
	seq    int
	phase  protocol.Phase
	usage  protocol.Usage
	rounds int
	branch string // branch for the execute phase
	result protocol.Result

	checks    []string         // verification commands (execute phase)
	baseline  []protocol.Check // the checks before any change
	fixRounds int              // verification failures sent back to the model

	lastInput    int // prompt tokens of the last model call
	lastMsgCount int // messages in that call

	repo     gitx.Git       // the workspace's repository (the user's checkout)
	worktree string         // the worktree directory, when Config.Worktree
	guard    *gitx.Snapshot // git config and hooks at the start
}

// Run executes the task. Failures of the task itself (the model gave up, a
// budget ran out, the model API failed) are reported in the Result; the
// error is only for setup problems (*SetupError) and for failing to commit
// or write the checkpoint at the end.
func Run(ctx context.Context, cfg Config) (*protocol.Result, error) {
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = defaultSteps
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = defaultTokens
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.Tools == nil {
		cfg.Tools = tools.Default()
	}
	if cfg.PlanTools == nil {
		cfg.PlanTools = tools.Planning()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxFixAttempts == 0 {
		cfg.MaxFixAttempts = defaultFixAttempts
	}
	if cfg.MaxFixAttempts < 0 {
		cfg.MaxFixAttempts = 0
	}
	if cfg.CheckTimeout <= 0 {
		cfg.CheckTimeout = defaultCheckTimeout
	}
	if cfg.ContextTokens <= 0 {
		cfg.ContextTokens = defaultContextTokens
	}
	cfg.CommitName = firstNonEmpty(cfg.CommitName, "Zenith")
	cfg.CommitEmail = firstNonEmpty(cfg.CommitEmail, "zenith@noreply.divmora.com")
	if cp := cfg.Resume; cp != nil {
		cfg.Task = cp.Task
		cfg.Persona = cp.Persona
	}

	r := &runner{cfg: cfg, repo: gitx.Git{Dir: cfg.ToolEnv.WS.Root}}
	r.git = r.repo
	r.result = protocol.Result{
		V:         protocol.Version,
		Task:      cfg.Task,
		Persona:   personaName(cfg.Persona),
		Model:     cfg.Model.Name(),
		StartedAt: time.Now().UTC(),
		Limits: protocol.Limits{MaxSteps: cfg.MaxSteps, MaxTokens: cfg.MaxTokens, TimeoutS: int64(cfg.Timeout / time.Second),
			MaxFixAttempts: cfg.MaxFixAttempts, ContextTokens: cfg.ContextTokens},
	}
	req, warnings, err := r.setup(ctx)
	if err != nil {
		r.removeWorktree()
		return nil, &SetupError{Err: err}
	}
	r.emit(protocol.Event{Type: protocol.EventTaskStarted, Model: r.result.Model, Task: &protocol.TaskInfo{
		Task: cfg.Task, Workspace: cfg.ToolEnv.WS.Root, Model: r.result.Model, BaseSHA: r.result.BaseSHA, Branch: r.result.Branch,
		Worktree: r.worktree, Persona: r.result.Persona,
	}})
	for _, w := range warnings {
		r.emit(protocol.Event{Type: protocol.EventWarning, Text: w})
	}
	if cp := cfg.Resume; cp != nil {
		how := "approved"
		if cfg.ApproveNotes != "" {
			how = "approved with notes: " + cfg.ApproveNotes
		}
		if !cfg.Approve {
			how = "feedback: " + cfg.Feedback
		}
		r.emit(protocol.Event{Type: protocol.EventResumed, Text: how})
	}

	runCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	set := cfg.Tools
	if r.phase == protocol.PhasePlan {
		set = cfg.PlanTools
	}
	var out outcome
	if r.phase == protocol.PhaseExecute && !r.runBaseline(runCtx, req) {
		r.stopCtx(ctx)
	} else {
		out = r.loop(runCtx, ctx, req, set)
	}

	// Finish with a fresh context: the run's own may have expired.
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer fcancel()
	// Undo anything the model's commands did to git config or hooks
	// before zenith's own git calls, and before the user's git sees them.
	if r.guard != nil {
		restored, err := r.guard.Restore()
		if len(restored) > 0 {
			r.emit(protocol.Event{Type: protocol.EventWarning, Text: "restored git files changed during the run: " + strings.Join(restored, ", ")})
		}
		if err != nil {
			r.cfg.Logger.Warn("restore git files", "error", err)
		}
	}
	var endErr error
	switch {
	case r.phase == protocol.PhasePlan && out.plan != nil:
		endErr = r.saveCheckpoint(req, *out.plan)
	case r.phase == protocol.PhaseExecute:
		// Commit even when the run stopped early, so partial work is
		// kept on the branch; the status says whether it is complete.
		endErr = r.commit(fctx)
	}
	if r.phase == protocol.PhasePlan {
		r.removeWorktree() // planning changed nothing; keep no checkout around
	}
	r.result.Usage = r.usage
	r.result.FinishedAt = time.Now().UTC()
	if endErr != nil {
		r.result.Status = protocol.StatusFailed
		r.result.Error = strings.TrimSpace(r.result.Error + "\n" + endErr.Error())
	}
	res := r.result
	r.emit(protocol.Event{Type: protocol.EventTaskFinished, Result: &res})
	return &res, endErr
}

// setup checks the repository, picks the phase, creates the branch for the
// execute phase, and builds the first request.
func (r *runner) setup(ctx context.Context) (*llm.Request, []string, error) {
	cfg := r.cfg
	if strings.TrimSpace(cfg.Task) == "" {
		return nil, nil, errors.New("task is empty")
	}
	cp := cfg.Resume
	switch {
	case cp != nil && cfg.Approve == (strings.TrimSpace(cfg.Feedback) != ""):
		return nil, nil, errors.New("resume needs exactly one of approve or feedback")
	case cp != nil && cp.V != protocol.CheckpointVersion:
		return nil, nil, fmt.Errorf("checkpoint version %d is not supported (want %d)", cp.V, protocol.CheckpointVersion)
	case cfg.ApproveNotes != "" && (cp == nil || !cfg.Approve):
		return nil, nil, errors.New("approval notes need a resume with approve")
	case cp != nil && len(cp.Messages) == 0:
		return nil, nil, errors.New("checkpoint has no conversation")
	}
	planning := (cp == nil && cfg.PlanFirst) || (cp != nil && !cfg.Approve)
	if planning && cfg.CheckpointPath == "" {
		return nil, nil, errors.New("a checkpoint path is required to stop for plan approval")
	}

	if !r.repo.IsRepo(ctx) {
		return nil, nil, fmt.Errorf("%s is not a git repository", cfg.ToolEnv.WS.Root)
	}
	head, err := r.repo.Head(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("the repository has no commits yet: %w", err)
	}
	base := head
	if cp != nil {
		base = cp.BaseSHA
	}
	var warnings []string
	dirty, err := r.repo.Dirty(ctx)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case cfg.Worktree:
		// The checkout is left alone, so it may be anywhere and dirty;
		// only the base commit has to exist.
		if _, err := r.repo.Run(ctx, "cat-file", "-e", base+"^{commit}"); err != nil {
			return nil, nil, fmt.Errorf("commit %s is not in this repository; fetch it first", short(base))
		}
		if dirty && cp == nil {
			warnings = append(warnings, "your checkout has uncommitted changes; the worktree starts from "+short(base)+" and does not include them")
		}
	case cp != nil && head != base:
		return nil, nil, fmt.Errorf("the workspace is at %s but the checkpoint was planned at %s; check out that commit first, or use a worktree", short(head), short(base))
	case dirty && !cfg.AllowDirty:
		return nil, nil, errors.New("the work tree has uncommitted changes; commit or stash them, pass --allow-dirty, or use a worktree")
	}
	r.result.BaseSHA = base
	r.result.Workspace = r.repo.Dir
	r.result.BaseBranch = r.repo.CurrentBranch(ctx)
	if cp != nil {
		r.result.BaseBranch = cp.BaseBranch
	}

	r.branch = cfg.Branch
	if r.branch == "" && cp != nil {
		r.branch = cp.Branch
	}
	if r.branch == "" {
		r.branch = "zenith/" + slug(cfg.Task) + "-" + randHex(3)
	}
	if _, err := r.repo.Run(ctx, "check-ref-format", "--branch", r.branch); err != nil {
		return nil, nil, fmt.Errorf("invalid branch name %q", r.branch)
	}
	if !planning {
		if _, err := r.repo.Run(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+r.branch); err == nil {
			return nil, nil, fmt.Errorf("branch %s already exists", r.branch)
		}
	}

	if r.guard, err = r.repo.Snapshot(ctx); err != nil {
		return nil, nil, fmt.Errorf("snapshot git config: %w", err)
	}
	if cfg.Worktree {
		if err := r.addWorktree(ctx, base, planning); err != nil {
			return nil, nil, err
		}
	}

	req := &llm.Request{System: prompt.System(r.cfg.ToolEnv.WS.Root, cfg.Persona), Cache: true}
	if cp != nil {
		r.usage = cp.Usage
		r.rounds = cp.Rounds
		r.result.Plan = cp.Plan
		req.Messages = append([]llm.Message(nil), cp.Messages...)
	}
	switch {
	case cp == nil && !planning:
		req.Messages = []llm.Message{{Role: llm.User, Text: prompt.Task(cfg.Task)}}
	case cp == nil:
		req.Messages = []llm.Message{{Role: llm.User, Text: prompt.TaskPlanFirst(cfg.Task)}}
	case cfg.Approve:
		req.Messages = append(req.Messages, llm.Message{Role: llm.User, Text: prompt.Approved(cfg.ApproveNotes)})
	default:
		req.Messages = append(req.Messages, llm.Message{Role: llm.User, Text: prompt.Feedback(cfg.Feedback)})
	}

	if planning {
		r.phase = protocol.PhasePlan
		req.Tools = cfg.PlanTools.Defs()
		return req, warnings, nil
	}
	r.phase = protocol.PhaseExecute
	req.Tools = cfg.Tools.Defs()
	switch {
	case cfg.NoVerify:
	case cfg.Verify != nil:
		r.checks = cfg.Verify
	default:
		r.checks = verify.Detect(r.cfg.ToolEnv.WS.Root)
	}
	if !cfg.Worktree {
		if _, err := r.git.Run(ctx, "switch", "--quiet", "--create", r.branch); err != nil {
			return nil, nil, fmt.Errorf("create branch %s: %w", r.branch, err)
		}
	}
	r.result.Branch = r.branch
	r.result.Worktree = r.worktree
	return req, warnings, nil
}

// addWorktree creates the worktree at base, on the new branch for the
// execute phase or detached for planning, and points the tools at it.
func (r *runner) addWorktree(ctx context.Context, base string, planning bool) error {
	dir := r.cfg.WorktreeDir
	if dir == "" {
		d, err := DefaultWorktreeDir(r.cfg.ToolEnv.WS.Root, r.branch, planning)
		if err != nil {
			return err
		}
		dir = d
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if rel, err := filepath.Rel(r.cfg.ToolEnv.WS.Root, dir); err == nil && !strings.HasPrefix(rel, "..") {
		return fmt.Errorf("worktree directory %s is inside the repository; choose one outside it", dir)
	}
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("worktree directory %s already exists", dir)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	args := []string{"worktree", "add", "--quiet"}
	if planning {
		args = append(args, "--detach", dir, base)
	} else {
		args = append(args, "-b", r.branch, dir, base)
	}
	if _, err := r.repo.Run(ctx, args...); err != nil {
		return fmt.Errorf("create worktree: %w", err)
	}
	r.worktree = dir
	ws, err := policy.NewWorkspace(dir)
	if err != nil {
		return err
	}
	env := *r.cfg.ToolEnv
	env.WS = ws
	r.cfg.ToolEnv = &env
	r.git = gitx.Git{Dir: ws.Root}
	return nil
}

// removeWorktree deletes the worktree (after planning, or a failed setup).
func (r *runner) removeWorktree() {
	if r.worktree == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := r.repo.Run(ctx, "worktree", "remove", "--force", r.worktree); err != nil {
		r.cfg.Logger.Warn("remove worktree", "dir", r.worktree, "error", err)
	}
	r.worktree = ""
}

// DefaultWorktreeDir is where worktrees go when no directory is given:
// the user cache directory, never inside the repository.
func DefaultWorktreeDir(repoRoot, branch string, planning bool) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("no cache directory for worktrees (pass a directory): %w", err)
	}
	name := filepath.Base(repoRoot) + "-" + strings.ReplaceAll(strings.TrimPrefix(branch, "zenith/"), "/", "-")
	if planning {
		name += "-plan-" + randHex(3)
	}
	return filepath.Join(cache, "zenith", "worktrees", name), nil
}

// outcome is how a loop ended: with finish, with a plan, or stopped (the
// status is then already set).
type outcome struct {
	finish *tools.FinishInput
	plan   *string
}

// loop runs the agent until its phase ends (finish or propose_plan), a
// budget, or a failure, and fills in the status. parent is the caller's
// context, to tell a timeout from a cancellation.
func (r *runner) loop(ctx, parent context.Context, req *llm.Request, set *tools.Set) outcome {
	nudge, terminal := prompt.Nudge, tools.Finish
	if r.phase == protocol.PhasePlan {
		nudge, terminal = prompt.NudgePlan, tools.ProposePlan
	}
	nudges := 0
	for {
		switch {
		case r.result.Steps >= r.cfg.MaxSteps:
			r.stop(protocol.StatusBudgetExceeded, fmt.Sprintf("step limit reached (%d model calls)", r.cfg.MaxSteps))
			return outcome{}
		case r.usage.Total() >= r.cfg.MaxTokens:
			r.stop(protocol.StatusBudgetExceeded, fmt.Sprintf("token limit reached (%d of %d)", r.usage.Total(), r.cfg.MaxTokens))
			return outcome{}
		case ctx.Err() != nil:
			r.stopCtx(parent)
			return outcome{}
		}

		r.maybeCompact(ctx, req)
		r.result.Steps++
		r.emit(protocol.Event{Type: protocol.EventModelCallStart, Step: r.result.Steps})
		r.lastMsgCount = len(req.Messages)
		resp, err := r.cfg.Model.Chat(ctx, req, r.cfg.OnDelta)
		if err != nil {
			if ctx.Err() != nil {
				r.stopCtx(parent)
			} else {
				r.stop(protocol.StatusFailed, "model call failed: "+err.Error())
			}
			return outcome{}
		}
		r.addUsage(resp.Usage)
		r.lastInput = resp.Usage.InputTokens
		req.Messages = append(req.Messages, resp.Message)
		text := cleanText(resp.Message.Text)
		if text != "" {
			r.emit(protocol.Event{Type: protocol.EventAssistantText, Step: r.result.Steps, Text: text})
		}

		if len(resp.Message.ToolCalls) == 0 {
			if resp.StopReason == llm.StopMaxTokens {
				req.Messages = append(req.Messages, llm.Message{Role: llm.User, Text: prompt.Continue})
				continue
			}
			if nudges >= maxNudges && text != "" {
				// The model keeps answering in prose: take its last
				// answer as the summary (or plan) rather than loop forever.
				if r.phase == protocol.PhasePlan {
					return r.planned(text)
				}
				r.result.Summary = text
				if report, again := r.verifyFinished(ctx, parent); again {
					nudges = 0
					req.Messages = append(req.Messages, llm.Message{Role: llm.User, Text: report})
					continue
				}
				return outcome{}
			}
			nudges++
			req.Messages = append(req.Messages, llm.Message{Role: llm.User, Text: nudge})
			continue
		}

		results, end := r.runTools(ctx, set, terminal, resp.Message.ToolCalls)
		req.Messages = append(req.Messages, llm.Message{Role: llm.User, ToolResults: results})
		if end == nil {
			continue
		}
		if r.phase == protocol.PhasePlan {
			var in tools.PlanInput
			_ = json.Unmarshal(end, &in)
			return r.planned(in.Plan)
		}
		var in tools.FinishInput
		_ = json.Unmarshal(end, &in)
		r.result.Summary = cleanText(in.Summary)
		if !in.Success {
			r.result.Status = protocol.StatusFailed
			r.result.Error = "the agent reported that it could not complete the task"
			return outcome{finish: &in}
		}
		if report, again := r.verifyFinished(ctx, parent); again {
			// Not done yet: replace finish's result and add the failures,
			// in the same user turn so roles keep alternating.
			last := &req.Messages[len(req.Messages)-1]
			for i := range last.ToolResults {
				if last.ToolResults[i].Name == tools.Finish {
					last.ToolResults[i].Content = "Not finished: zenith's verification failed; see below."
					last.ToolResults[i].IsError = true
				}
			}
			last.Text = report
			continue
		}
		return outcome{finish: &in}
	}
}

func (r *runner) planned(plan string) outcome {
	plan = cleanText(plan)
	r.rounds++
	r.result.Status = protocol.StatusAwaitingApproval
	r.result.Plan = plan
	r.result.Checkpoint = r.cfg.CheckpointPath
	r.emit(protocol.Event{Type: protocol.EventPlanProposed, Step: r.result.Steps, Text: plan})
	return outcome{plan: &plan}
}

// runTools runs the calls in order. A valid terminal call (finish or
// propose_plan) ends the phase after the other calls in the same reply have
// run; its input is returned.
func (r *runner) runTools(ctx context.Context, set *tools.Set, terminal string, calls []llm.ToolCall) ([]llm.ToolResult, json.RawMessage) {
	var end json.RawMessage
	results := make([]llm.ToolResult, 0, len(calls))
	for _, c := range calls {
		r.emit(protocol.Event{Type: protocol.EventToolCall, Step: r.result.Steps, Tool: &protocol.ToolEvent{ID: c.ID, Name: c.Name, Input: c.Input}})
		start := time.Now()
		var out string
		var isErr bool
		if ctx.Err() != nil {
			out, isErr = "skipped: the run was stopped", true
		} else {
			out, isErr = set.Call(ctx, r.cfg.ToolEnv, c)
		}
		r.emit(protocol.Event{Type: protocol.EventToolResult, Step: r.result.Steps, Tool: &protocol.ToolEvent{
			ID: c.ID, Name: c.Name, Output: out, IsError: isErr, DurationMS: time.Since(start).Milliseconds(),
		}})
		results = append(results, llm.ToolResult{CallID: c.ID, Name: c.Name, Content: out, IsError: isErr})
		r.recordCommand(c)
		if c.Name == terminal && !isErr && end == nil {
			end = c.Input
		}
	}
	return results, end
}

// maxCommands caps the commands listed in the result (and so in a PR body).
const maxCommands = 100

// recordCommand keeps the shell commands the agent ran, for the result.
func (r *runner) recordCommand(c llm.ToolCall) {
	if c.Name != "run_command" || len(r.result.Commands) >= maxCommands {
		return
	}
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(c.Input, &in) == nil && strings.TrimSpace(in.Command) != "" {
		r.result.Commands = append(r.result.Commands, strings.TrimSpace(in.Command))
	}
}

// saveCheckpoint writes the conversation so far, so `resume` can continue
// it later, possibly in a fresh sandbox.
func (r *runner) saveCheckpoint(req *llm.Request, plan string) error {
	cp := protocol.Checkpoint{
		V:          protocol.CheckpointVersion,
		Task:       r.cfg.Task,
		Model:      firstNonEmpty(r.cfg.ModelSpec, r.cfg.Model.Name()),
		BaseSHA:    r.result.BaseSHA,
		Branch:     r.branch,
		BaseBranch: r.result.BaseBranch,
		Plan:       plan,
		Persona:    r.cfg.Persona,
		Messages:   req.Messages,
		Usage:      r.usage,
		Rounds:     r.rounds,
		CreatedAt:  time.Now().UTC(),
	}
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return fmt.Errorf("encode checkpoint: %w", err)
	}
	// Write atomically: a resume must never see half a file. The
	// conversation includes repository content, so keep it private.
	tmp, err := os.CreateTemp(filepath.Dir(r.cfg.CheckpointPath), ".checkpoint-*")
	if err != nil {
		return fmt.Errorf("write checkpoint: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write checkpoint: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write checkpoint: %w", err)
	}
	if err := os.Rename(tmp.Name(), r.cfg.CheckpointPath); err != nil {
		return fmt.Errorf("write checkpoint: %w", err)
	}
	return nil
}

// LoadCheckpoint reads a checkpoint file.
func LoadCheckpoint(path string) (*protocol.Checkpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cp protocol.Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, fmt.Errorf("checkpoint %s: %w", path, err)
	}
	return &cp, nil
}

func (r *runner) stop(status protocol.Status, msg string) {
	r.result.Status = status
	r.result.Error = msg
	r.cfg.Logger.Warn("run stopped", "status", status, "reason", msg)
}

func (r *runner) stopCtx(parent context.Context) {
	if parent.Err() != nil {
		r.stop(protocol.StatusFailed, "the run was cancelled")
		return
	}
	r.stop(protocol.StatusBudgetExceeded, fmt.Sprintf("time limit reached (%s)", r.cfg.Timeout))
}

func (r *runner) addUsage(u llm.Usage) {
	r.usage.InputTokens += u.InputTokens
	r.usage.OutputTokens += u.OutputTokens
	r.usage.CachedInputTokens += u.CachedInputTokens
	r.usage.CacheWriteTokens += u.CacheWriteTokens
	r.usage.ModelCalls++
	u2 := r.usage
	r.emit(protocol.Event{Type: protocol.EventUsage, Step: r.result.Steps, Usage: &u2})
}

// commit stages everything and commits it on the run's branch. The model
// may also have committed on its own; files are reported against the base.
func (r *runner) commit(ctx context.Context) error {
	if _, err := r.git.Run(ctx, "add", "--all"); err != nil {
		return fmt.Errorf("commit failed: %w", err)
	}
	staged, err := r.git.Run(ctx, "diff", "--cached", "--name-only")
	if err != nil {
		return fmt.Errorf("commit failed: %w", err)
	}
	if staged != "" {
		msg := commitMessage(r.cfg.Task, r.result.Persona, r.result.Summary, r.result.Status)
		_, err := r.git.Run(ctx,
			"-c", "user.name="+r.cfg.CommitName, "-c", "user.email="+r.cfg.CommitEmail,
			"commit", "--quiet", "--no-verify", "-m", msg)
		if err != nil {
			return fmt.Errorf("commit failed: %w", err)
		}
	}
	head, err := r.git.Head(ctx)
	if err != nil {
		return fmt.Errorf("commit failed: %w", err)
	}
	if head == r.result.BaseSHA {
		return nil // nothing changed
	}
	files, err := r.git.Run(ctx, "diff", "--name-only", r.result.BaseSHA, head)
	if err != nil {
		return fmt.Errorf("commit failed: %w", err)
	}
	r.result.CommitSHA = head
	if files != "" {
		r.result.Files = strings.Split(files, "\n")
	}
	subject, _, _ := strings.Cut(commitMessage(r.cfg.Task, r.result.Persona, "", r.result.Status), "\n")
	r.emit(protocol.Event{Type: protocol.EventCommitted, Commit: &protocol.Commit{
		SHA: head, Branch: r.result.Branch, Message: subject, Files: r.result.Files,
	}})
	return nil
}

func commitMessage(task, persona, summary string, status protocol.Status) string {
	subject := strings.TrimSpace(strings.SplitN(strings.TrimSpace(task), "\n", 2)[0])
	if len(subject) > 64 {
		subject = strings.TrimSpace(subject[:61]) + "..."
	}
	prefix := "zenith"
	if persona != "" {
		prefix += "(" + persona + ")"
	}
	if status != protocol.StatusDone {
		prefix += " (incomplete)"
	}
	prefix += ": "
	msg := prefix + subject
	if summary != "" {
		msg += "\n\n" + summary
	}
	return msg
}

var thinkRE = regexp.MustCompile(`(?s)<think>.*?</think>`)

// cleanText removes reasoning tags some models leak into their answer text.
func cleanText(s string) string {
	s = thinkRE.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "<think>", "")
	s = strings.ReplaceAll(s, "</think>", "")
	return strings.TrimSpace(s)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(task string) string {
	s := nonSlug.ReplaceAllString(strings.ToLower(task), "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	if s == "" {
		s = "task"
	}
	return s
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func (r *runner) emit(e protocol.Event) {
	if r.cfg.Emit == nil {
		return
	}
	r.seq++
	e.V = protocol.Version
	e.Seq = r.seq
	e.Time = time.Now().UTC()
	e.Phase = r.phase
	r.cfg.Emit(e)
}

func personaName(p *protocol.Persona) string {
	if p == nil {
		return ""
	}
	return p.Name
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
