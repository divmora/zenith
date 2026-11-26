package harness

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/divmora/zenith/internal/policy"
	"github.com/divmora/zenith/internal/tools"
	"github.com/divmora/zenith/pkg/llm"
	"github.com/divmora/zenith/pkg/llm/llmtest"
	"github.com/divmora/zenith/pkg/protocol"
)

// repo makes a git repository with one commit.
func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init")
	return dir
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func config(t *testing.T, dir string, model llm.Client, events *[]protocol.Event) Config {
	t.Helper()
	ws, err := policy.NewWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		Task:    "Add a calc package",
		Model:   model,
		Tools:   tools.Default(),
		ToolEnv: tools.DefaultEnv(ws, policy.CommandEnv(nil)),
		Emit:    func(e protocol.Event) { *events = append(*events, e) },
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func finish(summary string, ok bool) llmtest.Step {
	return llmtest.Call("f", "finish", map[string]any{"summary": summary, "success": ok})
}

func TestRunHappyPath(t *testing.T) {
	dir := repo(t)
	model := llmtest.New(
		llmtest.Call("1", "list_files", map[string]any{}),
		llmtest.Call("2", "write_file", map[string]any{"path": "calc/calc.go", "content": "package calc\n\nfunc Add(a, b int) int { return a + b }\n"}),
		llmtest.Step{Reply: func(req *llm.Request) (*llm.Response, error) {
			last := req.Messages[len(req.Messages)-1]
			if len(last.ToolResults) != 1 || !strings.Contains(last.ToolResults[0].Content, "Created calc/calc.go") {
				return nil, errors.New("write result not sent back")
			}
			return &llm.Response{Message: llm.Message{Text: "Now checking. <think>hmm</think>", ToolCalls: []llm.ToolCall{
				{ID: "3", Name: "run_command", Input: []byte(`{"command":"cat calc/calc.go"}`)},
			}}}, nil
		}},
		finish("Added calc.Add.", true),
	)
	var events []protocol.Event
	res, err := Run(context.Background(), config(t, dir, model, &events))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != protocol.StatusDone || res.Summary != "Added calc.Add." || res.Steps != 4 {
		t.Fatalf("result = %+v", res)
	}
	if !strings.HasPrefix(res.Branch, "zenith/add-a-calc-package-") {
		t.Errorf("branch = %q", res.Branch)
	}
	if res.CommitSHA == "" || len(res.Files) != 1 || res.Files[0] != "calc/calc.go" {
		t.Errorf("commit = %s files = %v", res.CommitSHA, res.Files)
	}
	if got := gitRun(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != res.Branch {
		t.Errorf("checked-out branch = %q, want %q", got, res.Branch)
	}
	msg := gitRun(t, dir, "log", "-1", "--format=%an <%ae>%n%B")
	if !strings.HasPrefix(msg, "Zenith <zenith@noreply.divmora.com>\nzenith: Add a calc package\n\nAdded calc.Add.") {
		t.Errorf("commit message:\n%s", msg)
	}
	if gitRun(t, dir, "status", "--porcelain") != "" {
		t.Errorf("work tree not clean after commit")
	}

	if events[0].Type != protocol.EventTaskStarted || events[len(events)-1].Type != protocol.EventTaskFinished {
		t.Errorf("first/last events: %s, %s", events[0].Type, events[len(events)-1].Type)
	}
	for i, e := range events {
		if e.Seq != i+1 || e.V != protocol.Version {
			t.Fatalf("event %d has seq %d v %d", i, e.Seq, e.V)
		}
	}
	var sawText, sawCommit bool
	for _, e := range events {
		if e.Type == protocol.EventAssistantText && e.Text == "Now checking." {
			sawText = true
		}
		if e.Type == protocol.EventCommitted {
			sawCommit = true
		}
	}
	if !sawText || !sawCommit {
		t.Errorf("missing assistant_text (think tags stripped) or committed event")
	}
	if model.Remaining() != 0 {
		t.Errorf("%d scripted steps unused", model.Remaining())
	}
	if reqs := model.Requests(); !reqs[0].Cache || len(reqs[0].Tools) != 7 || !strings.Contains(reqs[0].System, "You are Zenith") {
		t.Errorf("first request not set up as expected")
	}
}

func TestRunStepBudgetKeepsPartialWork(t *testing.T) {
	dir := repo(t)
	model := llmtest.New(
		llmtest.Call("1", "write_file", map[string]any{"path": "half.txt", "content": "wip\n"}),
		llmtest.Call("2", "list_files", map[string]any{}),
	)
	var events []protocol.Event
	cfg := config(t, dir, model, &events)
	cfg.MaxSteps = 2
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != protocol.StatusBudgetExceeded || res.Status.ExitCode() != protocol.ExitBudgetExceeded || !strings.Contains(res.Error, "step limit") {
		t.Fatalf("result = %+v", res)
	}
	if res.CommitSHA == "" || !strings.HasPrefix(gitRun(t, dir, "log", "-1", "--format=%s"), "zenith (incomplete):") {
		t.Errorf("partial work should be committed as incomplete")
	}
}

func TestRunTokenBudget(t *testing.T) {
	dir := repo(t)
	step := llmtest.Call("1", "list_files", map[string]any{})
	step.Response.Usage = llm.Usage{InputTokens: 600, OutputTokens: 500}
	model := llmtest.New(step, step)
	var events []protocol.Event
	cfg := config(t, dir, model, &events)
	cfg.MaxTokens = 1000
	res, _ := Run(context.Background(), cfg)
	if res.Status != protocol.StatusBudgetExceeded || res.Steps != 1 || res.Usage.Total() != 1100 {
		t.Fatalf("result = %+v", res)
	}
}

func TestRunNudgesThenAcceptsProse(t *testing.T) {
	dir := repo(t)
	model := llmtest.New(llmtest.Text("I think it's done."), llmtest.Text("Really done."), llmtest.Text("All done: nothing to change."))
	var events []protocol.Event
	res, err := Run(context.Background(), config(t, dir, model, &events))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != protocol.StatusDone || res.Summary != "All done: nothing to change." || res.Steps != 3 {
		t.Fatalf("result = %+v", res)
	}
	if res.CommitSHA != "" || len(res.Files) != 0 {
		t.Errorf("nothing changed, so no commit: %+v", res)
	}
	reqs := model.Requests()
	if last := reqs[1].Messages[len(reqs[1].Messages)-1]; !strings.Contains(last.Text, "without calling finish") {
		t.Errorf("second request should carry the nudge, got %q", last.Text)
	}
}

func TestRunMaxTokensContinues(t *testing.T) {
	dir := repo(t)
	cut := llmtest.Text("Here is a very long")
	cut.Response.StopReason = llm.StopMaxTokens
	model := llmtest.New(cut, finish("ok", true))
	var events []protocol.Event
	res, _ := Run(context.Background(), config(t, dir, model, &events))
	if res.Status != protocol.StatusDone {
		t.Fatalf("result = %+v", res)
	}
	if last := model.Requests()[1].Messages; !strings.Contains(last[len(last)-1].Text, "cut off") {
		t.Errorf("expected a continue message")
	}
}

func TestRunFailures(t *testing.T) {
	t.Run("agent gives up", func(t *testing.T) {
		var events []protocol.Event
		res, _ := Run(context.Background(), config(t, repo(t), llmtest.New(finish("Can't: no tests exist.", false)), &events))
		if res.Status != protocol.StatusFailed || res.Status.ExitCode() != protocol.ExitFailed || res.Summary == "" {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("model error", func(t *testing.T) {
		var events []protocol.Event
		res, _ := Run(context.Background(), config(t, repo(t), llmtest.New(llmtest.Step{Err: errors.New("quota exceeded")}), &events))
		if res.Status != protocol.StatusFailed || !strings.Contains(res.Error, "quota exceeded") {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		var events []protocol.Event
		cfg := config(t, repo(t), blocking{}, &events)
		cfg.Timeout = 50 * time.Millisecond
		res, _ := Run(context.Background(), cfg)
		if res.Status != protocol.StatusBudgetExceeded || !strings.Contains(res.Error, "time limit") {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		var events []protocol.Event
		ctx, cancel := context.WithCancel(context.Background())
		cfg := config(t, repo(t), blocking{}, &events)
		cfg.Emit = func(e protocol.Event) {
			if e.Type == protocol.EventModelCallStart {
				cancel() // stop while the model call is in flight
			}
		}
		res, _ := Run(ctx, cfg)
		if res.Status != protocol.StatusFailed || !strings.Contains(res.Error, "cancelled") {
			t.Fatalf("result = %+v", res)
		}
	})
}

type blocking struct{}

func (blocking) Name() string { return "blocking" }
func (blocking) Chat(ctx context.Context, _ *llm.Request, _ llm.EventHandler) (*llm.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestRunSetupErrors(t *testing.T) {
	var events []protocol.Event
	dir := repo(t)
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), config(t, dir, llmtest.New(), &events))
	var se *SetupError
	if !errors.As(err, &se) || !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("dirty tree: %v", err)
	}
	cfg := config(t, dir, llmtest.New(finish("ok", true)), &events)
	cfg.AllowDirty = true
	if res, err := Run(context.Background(), cfg); err != nil || res.Status != protocol.StatusDone {
		t.Errorf("allow dirty: %v %+v", err, res)
	}

	if _, err := Run(context.Background(), config(t, t.TempDir(), llmtest.New(), &events)); !errors.As(err, &se) || !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("not a repo: %v", err)
	}
	cfg = config(t, repo(t), llmtest.New(), &events)
	cfg.Branch = "bad..name"
	if _, err := Run(context.Background(), cfg); !errors.As(err, &se) {
		t.Errorf("bad branch: %v", err)
	}
}

// The model can run any command, including ones that plant git hooks or
// config. zenith's own git calls must not execute them.
func TestRunIgnoresPlantedGitHooks(t *testing.T) {
	dir := repo(t)
	marker := filepath.Join(t.TempDir(), "pwned")
	plant := "mkdir -p .git/hooks && printf '#!/bin/sh\\ntouch " + marker + "\\n' > .git/hooks/pre-commit && chmod +x .git/hooks/pre-commit" +
		" && cp .git/hooks/pre-commit .git/hooks/post-commit" +
		" && git config core.fsmonitor .git/hooks/pre-commit" +
		" && echo change > file.txt"
	model := llmtest.New(
		llmtest.Call("1", "run_command", map[string]any{"command": plant}),
		finish("planted", true),
	)
	var events []protocol.Event
	res, err := Run(context.Background(), config(t, dir, model, &events))
	if err != nil {
		t.Fatal(err)
	}
	if res.CommitSHA == "" {
		t.Fatalf("expected a commit: %+v", res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook planted by the model ran during zenith's commit")
	}
}

func TestSlugAndClean(t *testing.T) {
	if got := slug("Fix the N+1 query in /api/users (urgent!)"); got != "fix-the-n-1-query-in-api-users-urgent" {
		t.Errorf("slug = %q", got)
	}
	if got := slug("!!!"); got != "task" {
		t.Errorf("slug = %q", got)
	}
	for in, want := range map[string]string{
		" <think> There are 3 files.": "There are 3 files.", // unclosed tag before the answer (Kimi)
		"<think>plan</think>Answer":   "Answer",             // closed block is reasoning
		"plain":                       "plain",
	} {
		if got := cleanText(in); got != want {
			t.Errorf("cleanText(%q) = %q, want %q", in, got, want)
		}
	}
}
