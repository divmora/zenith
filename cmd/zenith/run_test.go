package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/divmora/zenith/pkg/llm"
	"github.com/divmora/zenith/pkg/llm/llmtest"
	"github.com/divmora/zenith/pkg/protocol"
)

// fakeScript is what the "fake" provider replays in the next run.
var (
	fakeMu     sync.Mutex
	fakeScript []llmtest.Step
)

func init() {
	llm.Register("fake", func(llm.ModelConfig, *slog.Logger) (llm.Client, error) {
		fakeMu.Lock()
		defer fakeMu.Unlock()
		return llmtest.New(fakeScript...), nil
	})
}

func script(steps ...llmtest.Step) {
	fakeMu.Lock()
	defer fakeMu.Unlock()
	fakeScript = steps
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestRunCommand(t *testing.T) {
	dir := gitRepo(t)
	out := t.TempDir()
	script(
		llmtest.Call("1", "write_file", map[string]any{"path": "hello.txt", "content": "hi\n"}),
		llmtest.Call("2", "finish", map[string]any{"summary": "Added hello.txt.", "success": true}),
	)
	eventsPath := filepath.Join(out, "events.jsonl")
	resultPath := filepath.Join(out, "result.json")
	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--task", "Add hello.txt", "--workspace", dir, "--model", "fake:m",
		"--events", eventsPath, "--result", resultPath, "--branch", "zenith/test"}, &stdout, &stderr)
	if code != protocol.ExitDone {
		t.Fatalf("exit %d\nstderr:\n%s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout should be empty with --result: %q", stdout.String())
	}
	var res protocol.Result
	data, err := os.ReadFile(resultPath)
	if err != nil || json.Unmarshal(data, &res) != nil {
		t.Fatalf("result file: %v %s", err, data)
	}
	if res.Status != protocol.StatusDone || res.Branch != "zenith/test" || res.CommitSHA == "" || res.Summary != "Added hello.txt." {
		t.Errorf("result = %+v", res)
	}
	ef, err := os.Open(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ef.Close() }()
	var types []string
	sc := bufio.NewScanner(ef)
	for sc.Scan() {
		var e protocol.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("bad event line %q: %v", sc.Text(), err)
		}
		types = append(types, string(e.Type))
	}
	if len(types) < 4 || types[0] != "task_started" || types[len(types)-1] != "task_finished" {
		t.Errorf("event types = %v", types)
	}
	for _, want := range []string{"zenith: Add hello.txt", "→ write_file", "committed", "done after 2 step(s)"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("progress missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestRunEventsOnStdout(t *testing.T) {
	dir := gitRepo(t)
	script(llmtest.Call("1", "finish", map[string]any{"summary": "Nothing to do.", "success": true}))
	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--task", "noop", "--workspace", dir, "--model", "fake:m", "--events", "-", "--quiet"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	var last protocol.Event
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil || last.Type != protocol.EventTaskFinished || last.Result == nil {
		t.Errorf("last stdout line should be task_finished with the result: %s", lines[len(lines)-1])
	}
}

func TestRunExitCodes(t *testing.T) {
	dir := gitRepo(t)
	script(llmtest.Call("1", "finish", map[string]any{"summary": "No.", "success": false}))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run", "--task", "x", "--workspace", dir, "--model", "fake:m", "--quiet"}, &stdout, &stderr); code != protocol.ExitFailed {
		t.Errorf("failed task: exit %d", code)
	}
	var res protocol.Result
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil || res.Status != protocol.StatusFailed {
		t.Errorf("result on stdout: %v %q", err, stdout.String())
	}

	usage := map[string][]string{
		"no task":           {"run", "--workspace", dir, "--model", "fake:m"},
		"no model":          {"run", "--task", "x", "--workspace", dir, "--model", ""},
		"bad provider":      {"run", "--task", "x", "--workspace", dir, "--model", "nope:m"},
		"result inside":     {"run", "--task", "x", "--workspace", dir, "--model", "fake:m", "--result", filepath.Join(dir, "r.json")},
		"not a repo":        {"run", "--task", "x", "--workspace", t.TempDir(), "--model", "fake:m"},
		"both task sources": {"run", "--task", "x", "--task-file", "f", "--workspace", dir, "--model", "fake:m"},
		"extra args":        {"run", "--task", "x", "--model", "fake:m", "stray"},
	}
	for name, args := range usage {
		stderr.Reset()
		if code := run(args, &stdout, &stderr); code != protocol.ExitUsage {
			t.Errorf("%s: exit %d, want %d (stderr %q)", name, code, protocol.ExitUsage, stderr.String())
		}
	}
}

func TestPlanApproveFlow(t *testing.T) {
	dir := gitRepo(t)
	cp := filepath.Join(t.TempDir(), "plan.json")
	var stdout, stderr bytes.Buffer

	if code := run([]string{"run", "--task", "x", "--workspace", dir, "--model", "fake:m", "--approve-plan"}, &stdout, &stderr); code != protocol.ExitUsage {
		t.Errorf("--approve-plan without --checkpoint: exit %d", code)
	}

	script(llmtest.Call("1", "propose_plan", map[string]any{"plan": "Write a.txt"}))
	stderr.Reset()
	code := run([]string{"run", "--task", "Add a.txt", "--workspace", dir, "--model", "fake:m", "--approve-plan", "--checkpoint", cp}, &stdout, &stderr)
	if code != protocol.ExitAwaitingApproval {
		t.Fatalf("plan: exit %d\n%s", code, stderr.String())
	}
	for _, want := range []string{"proposed plan", "Write a.txt", "zenith resume --checkpoint " + cp + " --approve"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("plan progress missing %q:\n%s", want, stderr.String())
		}
	}

	script(llmtest.Call("2", "propose_plan", map[string]any{"plan": "Write a.txt with a greeting"}))
	stdout.Reset()
	if code := run([]string{"resume", "--checkpoint", cp, "--feedback", "Say hello in it", "--workspace", dir, "--quiet"}, &stdout, &stderr); code != protocol.ExitAwaitingApproval {
		t.Fatalf("feedback: exit %d", code)
	}
	var res protocol.Result
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil || res.Plan != "Write a.txt with a greeting" {
		t.Errorf("feedback result: %v %q", err, stdout.String())
	}

	script(
		llmtest.Call("3", "write_file", map[string]any{"path": "a.txt", "content": "hello\n"}),
		llmtest.Call("4", "finish", map[string]any{"summary": "Wrote a.txt.", "success": true}),
	)
	stdout.Reset()
	if code := run([]string{"resume", "--checkpoint", cp, "--approve", "--workspace", dir, "--quiet"}, &stdout, &stderr); code != protocol.ExitDone {
		t.Fatalf("approve: exit %d\n%s", code, stderr.String())
	}
	var done protocol.Result
	if err := json.Unmarshal(stdout.Bytes(), &done); err != nil || done.Status != protocol.StatusDone || done.CommitSHA == "" ||
		done.Plan != "Write a.txt with a greeting" || done.Checkpoint != "" {
		t.Errorf("approve result: %v %+v", err, done)
	}

	for name, args := range map[string][]string{
		"neither":          {"resume", "--checkpoint", cp, "--workspace", dir},
		"both":             {"resume", "--checkpoint", cp, "--approve", "--feedback", "x", "--workspace", dir},
		"no checkpoint":    {"resume", "--approve", "--workspace", dir},
		"missing file":     {"resume", "--checkpoint", filepath.Join(t.TempDir(), "nope.json"), "--approve", "--workspace", dir},
		"note w/o approve": {"resume", "--checkpoint", cp, "--feedback", "x", "--note", "n", "--workspace", dir},
		"cp in worktree":   {"run", "--task", "x", "--workspace", dir, "--model", "fake:m", "--approve-plan", "--checkpoint", filepath.Join(dir, "cp.json")},
	} {
		if code := run(args, &stdout, &stderr); code != protocol.ExitUsage {
			t.Errorf("%s: exit %d, want %d", name, code, protocol.ExitUsage)
		}
	}
}

func TestRunWorktreeFlag(t *testing.T) {
	dir := gitRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	script(
		llmtest.Call("1", "write_file", map[string]any{"path": "w.txt", "content": "w\n"}),
		llmtest.Call("2", "finish", map[string]any{"summary": "ok", "success": true}),
	)
	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--task", "Add w.txt", "--workspace", dir, "--model", "fake:m", "--worktree-dir", wt}, &stdout, &stderr)
	if code != protocol.ExitDone {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	var res protocol.Result
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil || res.Worktree != wt {
		t.Fatalf("result: %v %+v", err, res)
	}
	if _, err := os.Stat(filepath.Join(wt, "w.txt")); err != nil {
		t.Errorf("file not in worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "w.txt")); err == nil {
		t.Error("file leaked into the main checkout")
	}
	for _, want := range []string{"worktree  " + wt, "git worktree remove " + wt} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("progress missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestPersonaFlags(t *testing.T) {
	dir := gitRepo(t)
	script(llmtest.Step{Reply: func(req *llm.Request) (*llm.Response, error) {
		if !strings.Contains(req.System, "You are Bolt") || !strings.Contains(req.Messages[0].Text, "performance problem") {
			return &llm.Response{Message: llm.Message{ToolCalls: []llm.ToolCall{{ID: "f", Name: "finish", Input: []byte(`{"summary":"wrong prompt","success":false}`)}}}}, nil
		}
		return &llm.Response{Message: llm.Message{ToolCalls: []llm.ToolCall{{ID: "f", Name: "finish", Input: []byte(`{"summary":"Nothing slow found.","success":true}`)}}}}, nil
	}})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run", "--persona", "bolt", "--workspace", dir, "--model", "fake:m", "--quiet"}, &stdout, &stderr); code != protocol.ExitDone {
		t.Fatalf("persona with default task: exit %d %s %s", code, stdout.String(), stderr.String())
	}
	var res protocol.Result
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil || res.Persona != "bolt" || !strings.Contains(res.Task, "performance") {
		t.Errorf("result: %v %+v", err, res)
	}

	pf := filepath.Join(t.TempDir(), "custom.md")
	if err := os.WriteFile(pf, []byte("---\nname: custom\n---\nDo custom things.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"unknown persona":    {"run", "--persona", "jarvis", "--workspace", dir, "--model", "fake:m"},
		"both persona flags": {"run", "--persona", "bolt", "--persona-file", pf, "--workspace", dir, "--model", "fake:m"},
		"file without task":  {"run", "--persona-file", pf, "--workspace", dir, "--model", "fake:m"},
		"missing file":       {"run", "--persona-file", pf + ".nope", "--task", "x", "--workspace", dir, "--model", "fake:m"},
	} {
		if code := run(args, &stdout, &stderr); code != protocol.ExitUsage {
			t.Errorf("%s: exit %d", name, code)
		}
	}

	stdout.Reset()
	if code := run([]string{"personas"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "sentinel") || !strings.Contains(stdout.String(), "scribe") {
		t.Errorf("personas: %d %q", code, stdout.String())
	}
	stdout.Reset()
	if code := run([]string{"personas", "palette"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "WCAG") {
		t.Errorf("personas palette: %d", code)
	}
	if code := run([]string{"personas", "nope"}, &stdout, &stderr); code != protocol.ExitUsage {
		t.Errorf("personas nope: %d", code)
	}
}

func TestVerifyFlags(t *testing.T) {
	dir := gitRepo(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run", "--task", "x", "--workspace", dir, "--model", "fake:m", "--verify", "true", "--no-verify"}, &stdout, &stderr); code != protocol.ExitUsage {
		t.Errorf("--verify with --no-verify: exit %d", code)
	}
	script(llmtest.Call("1", "finish", map[string]any{"summary": "ok", "success": true}))
	stdout.Reset()
	code := run([]string{"run", "--task", "x", "--workspace", dir, "--model", "fake:m", "--quiet",
		"--verify", "true", "--verify", "test -d .git", "--max-fix-attempts", "0", "--context-tokens", "50000"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d %s", code, stderr.String())
	}
	var res protocol.Result
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil || len(res.Checks) != 4 || res.Limits.ContextTokens != 50000 || res.Limits.MaxFixAttempts != 0 {
		t.Errorf("result: %v %+v", err, res)
	}
}
