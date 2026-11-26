package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/divmora/zenith/pkg/llm/llmtest"
	"github.com/divmora/zenith/pkg/protocol"
)

// userCheckout makes a repo where the user is on their own branch with
// uncommitted work in progress.
func userCheckout(t *testing.T) string {
	t.Helper()
	dir := repo(t)
	gitRun(t, dir, "switch", "-q", "-c", "my-feature")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# my edits in progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func assertCheckoutUntouched(t *testing.T, dir string) {
	t.Helper()
	if got := gitRun(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "my-feature" {
		t.Errorf("user's branch changed to %q", got)
	}
	if got := gitRun(t, dir, "status", "--porcelain"); got != "M README.md\n?? notes.txt" {
		t.Errorf("user's uncommitted changes changed:\n%s", got)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "README.md")); string(data) != "# my edits in progress\n" {
		t.Errorf("user's file changed: %q", data)
	}
}

func TestWorktreeRunLeavesCheckoutAlone(t *testing.T) {
	dir := userCheckout(t)
	wt := filepath.Join(t.TempDir(), "wt")
	model := llmtest.New(
		llmtest.Call("1", "read_file", map[string]any{"path": "README.md"}),
		llmtest.Call("2", "write_file", map[string]any{"path": "README.md", "content": "# demo\n\nMore docs.\n"}),
		finish("Extended the README.", true),
	)
	var events []protocol.Event
	cfg := config(t, dir, model, &events)
	cfg.Worktree = true
	cfg.WorktreeDir = wt
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != protocol.StatusDone || res.Worktree != wt || res.CommitSHA == "" {
		t.Fatalf("result = %+v", res)
	}
	// The model saw the committed README, not the user's edits.
	if !strings.Contains(model.Requests()[1].Messages[2].ToolResults[0].Content, "# demo") {
		t.Errorf("worktree should start from HEAD, not the dirty checkout")
	}
	assertCheckoutUntouched(t, dir)
	if got := gitRun(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); got != res.Branch {
		t.Errorf("worktree branch = %q", got)
	}
	if got := gitRun(t, dir, "log", "-1", "--format=%s", res.Branch); !strings.HasPrefix(got, "zenith:") {
		t.Errorf("branch not visible from the main checkout: %q", got)
	}
	var warned bool
	for _, e := range events {
		if e.Type == protocol.EventWarning && strings.Contains(e.Text, "uncommitted changes") {
			warned = true
		}
	}
	if !warned {
		t.Error("expected a warning that uncommitted changes are not included")
	}
}

func TestWorktreePlanThenApproveAfterUserMovedOn(t *testing.T) {
	dir := userCheckout(t)
	var events []protocol.Event
	cfg := config(t, dir, llmtest.New(propose("Edit README")), &events)
	cfg.PlanFirst = true
	cfg.Worktree = true
	cfg.CheckpointPath = filepath.Join(t.TempDir(), "cp.json")
	planWT := filepath.Join(t.TempDir(), "plan-wt")
	cfg.WorktreeDir = planWT
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != protocol.StatusAwaitingApproval || res.Worktree != "" {
		t.Fatalf("plan result = %+v", res)
	}
	if _, err := os.Stat(planWT); err == nil {
		t.Error("planning worktree should be removed after the plan")
	}
	if list := gitRun(t, dir, "worktree", "list"); strings.Count(list, "\n") != 0 {
		t.Errorf("stale worktree registered:\n%s", list)
	}
	assertCheckoutUntouched(t, dir)

	// The user commits more work on their branch before approving.
	gitRun(t, dir, "add", "notes.txt")
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "user work")
	cp, _ := LoadCheckpoint(cfg.CheckpointPath)

	wt := filepath.Join(t.TempDir(), "wt")
	cfg = config(t, dir, llmtest.New(
		llmtest.Call("1", "write_file", map[string]any{"path": "out.txt", "content": "x\n"}),
		finish("done", true),
	), &events)
	cfg.Resume = cp
	cfg.Approve = true
	cfg.Worktree = true
	cfg.WorktreeDir = wt
	res, err = Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != protocol.StatusDone || res.BaseSHA != cp.BaseSHA {
		t.Fatalf("approve result = %+v", res)
	}
	// The branch starts from the planned commit, not the user's new one.
	if parent := gitRun(t, wt, "rev-parse", "HEAD~1"); parent != cp.BaseSHA {
		t.Errorf("branch parent = %s, want base %s", parent, cp.BaseSHA)
	}
	if _, err := os.Stat(filepath.Join(wt, "notes.txt")); err == nil {
		t.Error("the user's later commit leaked into the worktree")
	}
	if got := gitRun(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "my-feature" {
		t.Errorf("user's branch changed to %q", got)
	}
}

func TestWorktreeSetupErrors(t *testing.T) {
	dir := repo(t)
	var events []protocol.Event
	var se *SetupError

	cfg := config(t, dir, llmtest.New(), &events)
	cfg.Worktree = true
	cfg.WorktreeDir = filepath.Join(dir, "inside")
	if _, err := Run(context.Background(), cfg); !errors.As(err, &se) || !strings.Contains(err.Error(), "inside the repository") {
		t.Errorf("worktree inside repo: %v", err)
	}

	existing := t.TempDir()
	cfg.WorktreeDir = existing
	if _, err := Run(context.Background(), cfg); !errors.As(err, &se) || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("existing dir: %v", err)
	}

	gitRun(t, dir, "branch", "zenith/taken")
	cfg.WorktreeDir = filepath.Join(t.TempDir(), "wt")
	cfg.Branch = "zenith/taken"
	if _, err := Run(context.Background(), cfg); !errors.As(err, &se) || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("existing branch: %v", err)
	}
	if list := gitRun(t, dir, "worktree", "list"); strings.Count(list, "\n") != 0 {
		t.Errorf("failed setups left worktrees behind:\n%s", list)
	}
}

func TestDefaultWorktreeDir(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/cache")
	got, err := DefaultWorktreeDir("/src/myrepo", "zenith/fix-bug-abc123", false)
	if err != nil || got != "/cache/zenith/worktrees/myrepo-fix-bug-abc123" {
		t.Errorf("got %q %v", got, err)
	}
}

// Commands the model runs can rewrite .git/config and hooks, which the
// user's own git would later execute. They are restored after the run.
func TestRunRestoresGitConfigAndHooks(t *testing.T) {
	for _, worktree := range []bool{false, true} {
		t.Run(map[bool]string{false: "checkout", true: "worktree"}[worktree], func(t *testing.T) {
			dir := repo(t)
			cfgPath := filepath.Join(dir, ".git", "config")
			before, _ := os.ReadFile(cfgPath)
			model := llmtest.New(
				llmtest.Call("1", "run_command", map[string]any{"command": "git config core.fsmonitor /tmp/evil && " +
					"mkdir -p \"$(git rev-parse --git-common-dir)/hooks\" && " +
					"printf '#!/bin/sh\\n' > \"$(git rev-parse --git-common-dir)/hooks/pre-commit\" && echo x > f.txt"}),
				finish("ok", true),
			)
			var events []protocol.Event
			cfg := config(t, dir, model, &events)
			cfg.Worktree = worktree
			if worktree {
				cfg.WorktreeDir = filepath.Join(t.TempDir(), "wt")
			}
			res, err := Run(context.Background(), cfg)
			if err != nil || res.CommitSHA == "" {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			if after, _ := os.ReadFile(cfgPath); string(after) != string(before) {
				t.Errorf("git config not restored:\n%s", after)
			}
			if _, err := os.Stat(filepath.Join(dir, ".git", "hooks", "pre-commit")); err == nil {
				t.Error("planted hook not removed")
			}
			var warned bool
			for _, e := range events {
				if e.Type == protocol.EventWarning && strings.Contains(e.Text, "config") && strings.Contains(e.Text, "hooks/pre-commit") {
					warned = true
				}
			}
			if !warned {
				t.Error("no warning about the restored files")
			}
		})
	}
}
