package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/divmora/zenith/internal/policy"
	"github.com/divmora/zenith/pkg/llm"
)

func setup(t *testing.T, files map[string]string) *Env {
	t.Helper()
	root := t.TempDir()
	for p, c := range files {
		abs := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := policy.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	return DefaultEnv(ws, policy.CommandEnv(nil))
}

func call(t *testing.T, env *Env, name string, input any) (string, bool) {
	t.Helper()
	b, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return Default().Call(context.Background(), env, llm.ToolCall{ID: "1", Name: name, Input: b})
}

func TestReadFile(t *testing.T) {
	env := setup(t, map[string]string{"a.txt": "one\ntwo\nthree\n", "bin": "x\x00y", "empty": ""})
	out, isErr := call(t, env, "read_file", map[string]any{"path": "a.txt"})
	if isErr || out != "     1\tone\n     2\ttwo\n     3\tthree\n" {
		t.Errorf("read: %q %v", out, isErr)
	}
	out, _ = call(t, env, "read_file", map[string]any{"path": "a.txt", "offset": 2, "limit": 1})
	if !strings.HasPrefix(out, "     2\ttwo\n") || !strings.Contains(out, "offset=3") {
		t.Errorf("offset/limit: %q", out)
	}
	for _, p := range []string{"bin", "missing", "../x", "."} {
		if _, isErr := call(t, env, "read_file", map[string]any{"path": p}); !isErr {
			t.Errorf("read_file %q should fail", p)
		}
	}
	if out, _ := call(t, env, "read_file", map[string]any{"path": "empty"}); out != "(empty file)" {
		t.Errorf("empty: %q", out)
	}
}

func TestWriteAndEdit(t *testing.T) {
	env := setup(t, map[string]string{"a.go": "x := 1\ny := 1\n"})
	if out, isErr := call(t, env, "write_file", map[string]any{"path": "new/dir/b.go", "content": "package b\n"}); isErr || !strings.HasPrefix(out, "Created") {
		t.Errorf("write: %q", out)
	}
	if data, _ := os.ReadFile(filepath.Join(env.WS.Root, "new/dir/b.go")); string(data) != "package b\n" {
		t.Errorf("content = %q", data)
	}
	if _, isErr := call(t, env, "write_file", map[string]any{"path": ".git/hooks/pre-commit", "content": "evil"}); !isErr {
		t.Error("writing .git must fail")
	}
	out, isErr := call(t, env, "edit_file", map[string]any{"path": "a.go", "old_string": "1", "new_string": "2"})
	if !isErr || !strings.Contains(out, "occurs 2 times") {
		t.Errorf("ambiguous edit: %q", out)
	}
	if _, isErr := call(t, env, "edit_file", map[string]any{"path": "a.go", "old_string": "x := 1", "new_string": "x := 2"}); isErr {
		t.Error("unique edit failed")
	}
	if _, isErr := call(t, env, "edit_file", map[string]any{"path": "a.go", "old_string": "1", "new_string": "3", "replace_all": true}); isErr {
		t.Error("replace_all failed")
	}
	if data, _ := os.ReadFile(filepath.Join(env.WS.Root, "a.go")); string(data) != "x := 2\ny := 3\n" {
		t.Errorf("after edits: %q", data)
	}
	if out, isErr := call(t, env, "edit_file", map[string]any{"path": "a.go", "old_string": "nope", "new_string": "z"}); !isErr || !strings.Contains(out, "not found") {
		t.Errorf("missing old_string: %q", out)
	}
}

func TestListAndGrep(t *testing.T) {
	env := setup(t, map[string]string{
		"main.go":               "package main\n// TODO fix\n",
		"pkg/util/util.go":      "package util\nfunc Todo() {}\n",
		"web/app.ts":            "// TODO web\n",
		"node_modules/x/x.js":   "TODO dep\n",
		"ignored/gen.go":        "TODO generated\n",
		".gitignore":            "ignored/\n",
		"docs/readme.md":        "nothing\n",
		"pkg/util/util_test.go": "package util\n",
	})
	if _, err := exec.LookPath("git"); err == nil {
		cmd := exec.Command("git", "init", "-q", env.WS.Root)
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
	}
	out, _ := call(t, env, "list_files", map[string]any{"pattern": "*.go"})
	if !strings.Contains(out, "pkg/util/util.go") || strings.Contains(out, "ignored/gen.go") || strings.Contains(out, "app.ts") {
		t.Errorf("list *.go:\n%s", out)
	}
	out, _ = call(t, env, "list_files", map[string]any{"pattern": "pkg/**/*_test.go"})
	if strings.TrimSpace(out) != "pkg/util/util_test.go" {
		t.Errorf("list pkg/**:\n%s", out)
	}
	out, _ = call(t, env, "list_files", map[string]any{"path": "web"})
	if strings.TrimSpace(out) != "web/app.ts" {
		t.Errorf("list web:\n%s", out)
	}
	out, _ = call(t, env, "grep", map[string]any{"pattern": "todo", "case_insensitive": true, "glob": "*.{go,ts}"})
	for _, want := range []string{"main.go:2:// TODO fix", "pkg/util/util.go:2:func Todo() {}", "web/app.ts:1:// TODO web"} {
		if !strings.Contains(out, want) {
			t.Errorf("grep missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "generated") {
		t.Errorf("grep should skip ignored files:\n%s", out)
	}
	if out, isErr := call(t, env, "grep", map[string]any{"pattern": "("}); !isErr || !strings.Contains(out, "invalid pattern") {
		t.Errorf("bad regex: %q", out)
	}
}

func TestCompileGlob(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"*.go", "a/b/c.go", true},
		{"*.go", "a/b/c.ts", false},
		{"src/*.ts", "src/a.ts", true},
		{"src/*.ts", "src/x/a.ts", false},
		{"src/**/*.ts", "src/a.ts", true},
		{"src/**/*.ts", "src/x/y/a.ts", true},
		{"**/test_*.py", "a/test_x.py", true},
		{"*.{js,jsx}", "x.jsx", true},
		{"a?.txt", "ab.txt", true},
	}
	for _, c := range cases {
		m, err := compileGlob(c.glob)
		if err != nil {
			t.Fatal(err)
		}
		if got := m(c.path); got != c.want {
			t.Errorf("glob %q on %q = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
}

func TestRunCommand(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-secret")
	env := setup(t, map[string]string{"f.txt": "hello"})
	env.CommandEnv = policy.CommandEnv(nil)

	out, isErr := call(t, env, "run_command", map[string]any{"command": "cat f.txt; echo; echo oops >&2; exit 3"})
	if isErr || !strings.HasPrefix(out, "exit code: 3") || !strings.Contains(out, "hello") || !strings.Contains(out, "oops") {
		t.Errorf("run: %q", out)
	}
	out, _ = call(t, env, "run_command", map[string]any{"command": "env"})
	if strings.Contains(out, "sk-secret") || !strings.Contains(out, "CI=true") {
		t.Errorf("command env leaked secrets or missed CI:\n%s", out)
	}

	env.MaxCommandTimeout = 300 * time.Millisecond
	start := time.Now()
	out, isErr = call(t, env, "run_command", map[string]any{"command": "echo started; sleep 30 & sleep 30; echo never", "timeout_seconds": 60})
	if !isErr || !strings.Contains(out, "timed out") || !strings.Contains(out, "started") || time.Since(start) > 10*time.Second {
		t.Errorf("timeout: %q after %v", out, time.Since(start))
	}

	env.MaxOutput = 200
	env.MaxCommandTimeout = time.Minute
	out, _ = call(t, env, "run_command", map[string]any{"command": "yes line | head -n 5000; echo THE-END"})
	if len(out) > 300 || !strings.Contains(out, "THE-END") {
		t.Errorf("long output not truncated around the end (%d bytes): %q", len(out), out)
	}
}

func TestCallErrors(t *testing.T) {
	env := setup(t, nil)
	if out, isErr := Default().Call(context.Background(), env, llm.ToolCall{Name: "rm_rf"}); !isErr || !strings.Contains(out, "available tools") {
		t.Errorf("unknown tool: %q", out)
	}
	out, isErr := Default().Call(context.Background(), env, llm.ToolCall{Name: "read_file", Input: json.RawMessage(`{"_raw":"{\"path\":"}`)})
	if !isErr || !strings.Contains(out, "not valid JSON") {
		t.Errorf("broken args: %q", out)
	}
	if _, isErr := call(t, env, "finish", map[string]any{"summary": " ", "success": true}); !isErr {
		t.Error("finish without summary should fail")
	}
}

func TestCapped(t *testing.T) {
	c := &capped{max: 10}
	_, _ = c.Write([]byte("0123456789abcdefghij"))
	if got := c.String(); !strings.HasPrefix(got, "01234") || !strings.HasSuffix(got, "fghij") || !strings.Contains(got, "10 bytes dropped") {
		t.Errorf("got %q", got)
	}
}

func TestPlanningSetIsReadOnly(t *testing.T) {
	s := Planning()
	for _, d := range s.Defs() {
		tool, _ := s.Get(d.Name)
		if !tool.ReadOnly {
			t.Errorf("%s is in the planning set but not read-only", d.Name)
		}
	}
	if _, ok := s.Get("run_command"); ok {
		t.Error("run_command must not be available while planning")
	}
	env := setup(t, nil)
	if out, isErr := s.Call(context.Background(), env, llm.ToolCall{Name: "write_file", Input: json.RawMessage(`{"path":"x","content":"y"}`)}); !isErr || !strings.Contains(out, "unknown tool") {
		t.Errorf("write during planning: %q", out)
	}
	if out, isErr := s.Call(context.Background(), env, llm.ToolCall{Name: ProposePlan, Input: json.RawMessage(`{"plan":"1. do it"}`)}); isErr {
		t.Errorf("propose_plan: %q", out)
	}
}
