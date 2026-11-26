package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/divmora/zenith/internal/harness"
	"github.com/divmora/zenith/internal/persona"
	"github.com/divmora/zenith/internal/policy"
	"github.com/divmora/zenith/internal/tools"
	"github.com/divmora/zenith/pkg/llm"
	"github.com/divmora/zenith/pkg/protocol"
)

const runUsage = `Usage: zenith run --task "..." [flags]
       zenith run --persona NAME [--task "..."] [flags]

Run one task in a git repository: zenith creates a branch, lets the model
explore, edit and test, and commits the result on that branch.

With --persona (see zenith personas), the model works as a specialist
(security, performance, UX, maintainability, tests, docs) and the persona's
default task is used unless you pass --task.

With --worktree, all of that happens in a new git worktree instead, so your
checkout is never touched; the result's "worktree" field says where it is.

With --approve-plan, the model only explores (read-only) and proposes a
plan; zenith saves --checkpoint and exits 10. Continue with zenith resume.

Output: progress for humans on stderr; the result JSON on stdout (or in
--result). With --events, a JSON event per line (use "-" for stdout).
Exit codes: 0 done, 10 awaiting plan approval, 20 budget exceeded,
30 task failed, 1 internal error, 2 invalid usage.

Flags:
`

type runFlags struct {
	task, taskFile, workspace, model string
	approvePlan                      bool
	persona, personaFile             string
	checkpoint                       string
	approve                          bool
	feedback, feedbackFile, notes    string
	maxSteps, maxTokens              int
	timeout                          time.Duration
	events, result, branch           string
	allowDirty, quiet, noStream      bool
	worktree                         bool
	worktreeDir                      string
	personaInfo                      *protocol.Persona
	verify                           multiFlag
	noVerify                         bool
	maxFixAttempts, contextTokens    int
	checkTimeout                     time.Duration
	verbose                          bool
	passEnv                          string
}

// commonFlags registers the flags run and resume share.
func commonFlags(fs *flag.FlagSet, f *runFlags) {
	fs.StringVar(&f.workspace, "workspace", ".", "git repository to work in")
	fs.IntVar(&f.maxSteps, "max-steps", 100, "maximum model calls")
	fs.IntVar(&f.maxTokens, "max-tokens", 5_000_000, "maximum input+output tokens over the run")
	fs.DurationVar(&f.timeout, "timeout", time.Hour, "maximum run time")
	fs.StringVar(&f.events, "events", "", "write JSON events to this file (- for stdout)")
	fs.StringVar(&f.result, "result", "", "write the result JSON to this file instead of stdout")
	fs.StringVar(&f.branch, "branch", "", "branch to create (default zenith/<task-slug>-<random>)")
	fs.BoolVar(&f.allowDirty, "allow-dirty", false, "start even if the work tree has uncommitted changes (they are committed with the result)")
	fs.BoolVar(&f.worktree, "worktree", false, "work in a new git worktree; your checkout (branch, uncommitted changes) is not touched")
	fs.StringVar(&f.worktreeDir, "worktree-dir", "", "where to create the worktree (default: user cache dir, zenith/worktrees/...)")
	fs.StringVar(&f.passEnv, "pass-env", "", "comma-separated extra env vars that commands the model runs may see")
	fs.Var(&f.verify, "verify", "check command zenith runs after the agent finishes (repeatable; default: detected from the project, e.g. make test)")
	fs.BoolVar(&f.noVerify, "no-verify", false, "don't run checks after the agent finishes")
	fs.IntVar(&f.maxFixAttempts, "max-fix-attempts", 3, "times failing checks go back to the agent to fix (0 = report only)")
	fs.DurationVar(&f.checkTimeout, "check-timeout", 10*time.Minute, "timeout for each check command")
	fs.IntVar(&f.contextTokens, "context-tokens", 128_000, "the model's context window; the conversation is compacted before 75% of it")
	fs.BoolVar(&f.quiet, "quiet", false, "no progress on stderr")
	fs.BoolVar(&f.noStream, "no-stream", false, "use non-streaming model calls")
	fs.BoolVar(&f.verbose, "verbose", false, "debug logging on stderr")
}

func parse(fs *flag.FlagSet, args []string, stderr io.Writer) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return protocol.ExitDone, false
		}
		return protocol.ExitUsage, false
	}
	if fs.NArg() > 0 {
		return usageErr(stderr, "unexpected arguments: %s", strings.Join(fs.Args(), " ")), false
	}
	return 0, true
}

func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr, runUsage)
		fs.PrintDefaults()
	}
	var f runFlags
	fs.StringVar(&f.task, "task", "", "the task to do")
	fs.StringVar(&f.taskFile, "task-file", "", "read the task from a file (- for stdin)")
	fs.StringVar(&f.model, "model", os.Getenv("ZENITH_MODEL"), "provider:model, comma-separated for fallbacks (env ZENITH_MODEL)")
	fs.StringVar(&f.persona, "persona", "", "use a built-in persona (see zenith personas); its default task is used when --task is not given")
	fs.StringVar(&f.personaFile, "persona-file", "", "use a persona from a Markdown file (same format as the built-ins)")
	fs.BoolVar(&f.approvePlan, "approve-plan", false, "stop after a read-only planning phase and wait for approval (needs --checkpoint)")
	fs.StringVar(&f.checkpoint, "checkpoint", "", "where to save the checkpoint when stopping for approval")
	commonFlags(fs, &f)
	if code, ok := parse(fs, args, stderr); !ok {
		return code
	}
	var p *persona.Persona
	switch {
	case f.persona != "" && f.personaFile != "":
		return usageErr(stderr, "use --persona or --persona-file, not both")
	case f.persona != "":
		got, err := persona.Get(f.persona)
		if err != nil {
			return usageErr(stderr, "%v", err)
		}
		p = &got
	case f.personaFile != "":
		got, err := persona.Load(f.personaFile)
		if err != nil {
			return usageErr(stderr, "%v", err)
		}
		p = &got
	}
	if p != nil && f.task == "" && f.taskFile == "" {
		f.task = p.Task
		if f.task == "" {
			return usageErr(stderr, "persona %s has no default task; pass --task", p.Name)
		}
	}
	task, err := readTask(f.task, f.taskFile)
	if err != nil {
		return usageErr(stderr, "%v", err)
	}
	if p != nil {
		f.personaInfo = p.Info()
	}
	if f.noVerify && len(f.verify) > 0 {
		return usageErr(stderr, "use --verify or --no-verify, not both")
	}
	if f.approvePlan && f.checkpoint == "" {
		return usageErr(stderr, "--approve-plan needs --checkpoint FILE")
	}
	return execute(f, task, nil, stdout, stderr)
}

const resumeUsage = `Usage: zenith resume --checkpoint FILE (--approve | --feedback "...") [flags]

Continue a task that stopped for plan approval. The workspace must be at the
checkpoint's base commit with a clean work tree (a fresh clone is fine), or
pass --worktree: then only the base commit has to exist in the repository.

--approve      implement the plan, then commit on the branch (exit 0);
               add --note "..." for small corrections on top of the plan
--feedback     revise the plan; the checkpoint is updated and zenith exits 10
               again for another review

The model defaults to the one that made the plan. Flags:
`

func cmdResume(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr, resumeUsage)
		fs.PrintDefaults()
	}
	var f runFlags
	fs.StringVar(&f.checkpoint, "checkpoint", "", "checkpoint file from zenith run --approve-plan (updated on --feedback)")
	fs.BoolVar(&f.approve, "approve", false, "approve the plan and implement it")
	fs.StringVar(&f.feedback, "feedback", "", "ask for a revised plan with this feedback")
	fs.StringVar(&f.feedbackFile, "feedback-file", "", "read the feedback from a file (- for stdin)")
	fs.StringVar(&f.notes, "note", "", "with --approve: small corrections the agent should apply on top of the plan")
	fs.StringVar(&f.model, "model", "", "override the checkpoint's model (provider:model)")
	commonFlags(fs, &f)
	if code, ok := parse(fs, args, stderr); !ok {
		return code
	}
	if f.checkpoint == "" {
		return usageErr(stderr, "--checkpoint is required")
	}
	if f.feedbackFile != "" {
		fb, err := readTask(f.feedback, f.feedbackFile)
		if err != nil {
			return usageErr(stderr, "feedback: %v", err)
		}
		f.feedback = fb
	}
	if f.approve == (strings.TrimSpace(f.feedback) != "") {
		return usageErr(stderr, "pass exactly one of --approve or --feedback")
	}
	if f.notes != "" && !f.approve {
		return usageErr(stderr, "--note goes with --approve; use --feedback to ask for a new plan")
	}
	cp, err := harness.LoadCheckpoint(f.checkpoint)
	if err != nil {
		return usageErr(stderr, "%v", err)
	}
	if f.model == "" {
		f.model = cp.Model
	}
	return execute(f, cp.Task, cp, stdout, stderr)
}

// execute runs a task (new or resumed) and reports the result.
func execute(f runFlags, task string, cp *protocol.Checkpoint, stdout, stderr io.Writer) int {
	if f.model == "" {
		return usageErr(stderr, "no model: pass --model provider:model or set ZENITH_MODEL (providers: %s)", strings.Join(llm.Providers(), ", "))
	}
	ws, err := policy.NewWorkspace(f.workspace)
	if err != nil {
		return usageErr(stderr, "%v", err)
	}
	for _, p := range []string{f.events, f.result, f.checkpoint} {
		if insideWorkspace(ws.Root, p) {
			return usageErr(stderr, "%s is inside the workspace; it would be committed with the result. Write it elsewhere.", p)
		}
	}

	level := slog.LevelWarn
	if f.verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	var cfgs []llm.ModelConfig
	for _, spec := range strings.Split(f.model, ",") {
		cfg, err := llm.ParseSpec(spec)
		if err != nil {
			return usageErr(stderr, "%v", err)
		}
		cfg.Region = os.Getenv("AWS_REGION")
		cfgs = append(cfgs, cfg)
	}
	model, err := llm.NewChain(cfgs, logger)
	if err != nil {
		return usageErr(stderr, "%v", err)
	}

	// Event sinks.
	var sinks []func(protocol.Event)
	var eventsOut io.Writer
	if f.events == "-" {
		eventsOut = stdout
	} else if f.events != "" {
		ef, err := os.Create(f.events)
		if err != nil {
			return internalErr(stderr, "open events file: %v", err)
		}
		defer func() { _ = ef.Close() }()
		eventsOut = ef
	}
	if eventsOut != nil {
		enc := json.NewEncoder(eventsOut)
		sinks = append(sinks, func(e protocol.Event) { _ = enc.Encode(e) })
	}
	var prog *progress
	if !f.quiet {
		prog = &progress{w: stderr, resultPath: f.result}
		sinks = append(sinks, prog.event)
	}

	var extra []string
	for _, n := range strings.Split(f.passEnv, ",") {
		if n = strings.TrimSpace(n); n != "" {
			extra = append(extra, n)
		}
	}
	cfg := harness.Config{
		Task:           task,
		Persona:        f.personaInfo,
		Model:          model,
		ModelSpec:      f.model,
		PlanFirst:      f.approvePlan,
		CheckpointPath: f.checkpoint,
		Resume:         cp,
		Approve:        f.approve,
		Feedback:       f.feedback,
		ApproveNotes:   f.notes,
		Verify:         f.verify.list(),
		NoVerify:       f.noVerify,
		MaxFixAttempts: fixAttempts(f.maxFixAttempts),
		CheckTimeout:   f.checkTimeout,
		ContextTokens:  f.contextTokens,
		Worktree:       f.worktree || f.worktreeDir != "",
		WorktreeDir:    f.worktreeDir,
		Tools:          tools.Default(),
		ToolEnv:        tools.DefaultEnv(ws, policy.CommandEnv(extra)),
		MaxSteps:       f.maxSteps,
		MaxTokens:      f.maxTokens,
		Timeout:        f.timeout,
		Branch:         f.branch,
		AllowDirty:     f.allowDirty,
		Logger:         logger,
		Emit: func(e protocol.Event) {
			for _, s := range sinks {
				s(e)
			}
		},
	}
	if prog != nil && !f.noStream {
		cfg.OnDelta = prog.delta
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res, err := harness.Run(ctx, cfg)
	var setupErr *harness.SetupError
	if errors.As(err, &setupErr) {
		return usageErr(stderr, "%v", setupErr.Err)
	}
	if res == nil {
		return internalErr(stderr, "%v", err)
	}

	data, _ := json.MarshalIndent(res, "", "  ")
	switch {
	case f.result != "":
		if werr := os.WriteFile(f.result, append(data, '\n'), 0o644); werr != nil {
			return internalErr(stderr, "write result: %v", werr)
		}
	case f.events != "-":
		_, _ = fmt.Fprintln(stdout, string(data))
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "zenith: %v\n", err)
		return protocol.ExitInternal
	}
	return res.Status.ExitCode()
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, "; ") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// list returns nil when the flag was not given, so checks are detected.
func (m multiFlag) list() []string {
	if len(m) == 0 {
		return nil
	}
	return []string(m)
}

// fixAttempts maps the flag (0 = none) to the harness (0 = default, <0 = none).
func fixAttempts(n int) int {
	if n <= 0 {
		return -1
	}
	return n
}

func readTask(task, file string) (string, error) {
	switch {
	case task != "" && file != "":
		return "", errors.New("use --task or --task-file, not both")
	case file == "-":
		b, err := io.ReadAll(os.Stdin)
		return strings.TrimSpace(string(b)), err
	case file != "":
		b, err := os.ReadFile(file)
		return strings.TrimSpace(string(b)), err
	case strings.TrimSpace(task) == "":
		return "", errors.New("no task: pass --task \"...\" or --task-file")
	}
	return strings.TrimSpace(task), nil
}

func insideWorkspace(root, p string) bool {
	if p == "" || p == "-" {
		return false
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	if real, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		abs = filepath.Join(real, filepath.Base(abs))
	}
	rel, err := filepath.Rel(root, abs)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func usageErr(w io.Writer, format string, a ...any) int {
	_, _ = fmt.Fprintf(w, "zenith: "+format+"\n", a...)
	return protocol.ExitUsage
}

func internalErr(w io.Writer, format string, a ...any) int {
	_, _ = fmt.Fprintf(w, "zenith: "+format+"\n", a...)
	return protocol.ExitInternal
}
