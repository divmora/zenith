package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/divmora/zenith/pkg/llm/llmtest"
	"github.com/divmora/zenith/pkg/protocol"
)

// The full push and pull request flow is tested in internal/publish
// against a fake API and a local bare repository; here, the command's
// own checks, and that a run records what publish needs.
func TestPublishCommand(t *testing.T) {
	for _, name := range []string{"ZENITH_GITHUB_TOKEN", "GITHUB_TOKEN", "GH_TOKEN", "ZENITH_GITLAB_TOKEN", "GITLAB_TOKEN"} {
		t.Setenv(name, "")
	}
	dir := gitRepo(t)
	out := t.TempDir()
	script(
		llmtest.Call("1", "run_command", map[string]any{"command": "echo hi > hello.txt"}),
		llmtest.Call("2", "finish", map[string]any{"summary": "Added hello.txt.", "success": true}),
	)
	resultPath := filepath.Join(out, "result.json")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run", "--task", "Add hello.txt", "--workspace", dir, "--model", "fake:m", "--no-verify",
		"--result", resultPath, "--quiet"}, &stdout, &stderr); code != protocol.ExitDone {
		t.Fatalf("run exit %d: %s", code, stderr.String())
	}
	var res protocol.Result
	data, _ := os.ReadFile(resultPath)
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatal(err)
	}
	if res.Workspace != dir || res.BaseBranch != "main" || len(res.Commands) != 1 || res.Commands[0] != "echo hi > hello.txt" {
		t.Errorf("result lacks publish fields: workspace=%q base=%q commands=%q", res.Workspace, res.BaseBranch, res.Commands)
	}

	writeResult := func(mut func(*protocol.Result)) string {
		r := res
		mut(&r)
		b, _ := json.Marshal(r)
		p := filepath.Join(t.TempDir(), "r.json")
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	same := func(*protocol.Result) {}
	cases := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"no result", []string{"publish"}, nil, "--result is required"},
		{"awaiting", []string{"publish", "--result", writeResult(func(r *protocol.Result) { r.Status = protocol.StatusAwaitingApproval })}, nil, "awaiting approval"},
		{"failed", []string{"publish", "--result", writeResult(func(r *protocol.Result) { r.Status = protocol.StatusFailed })}, nil, "--allow-incomplete"},
		{"version", []string{"publish", "--result", writeResult(func(r *protocol.Result) { r.V = 99 })}, nil, "version 99"},
		{"no origin", []string{"publish", "--result", writeResult(same)}, nil, "no origin remote"},
		{"no token", []string{"publish", "--result", writeResult(same), "--remote", "git@github.com:o/r.git"}, nil, "GITHUB_TOKEN"},
		{"gitlab token", []string{"publish", "--result", writeResult(same), "--remote", "https://gitlab.com/g/p"}, nil, "GITLAB_TOKEN"},
		{"platform", []string{"publish", "--result", writeResult(same), "--remote", "https://git.example.com/o/r"}, nil, "--platform"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(c.args, &stdout, &stderr); code != protocol.ExitUsage || !strings.Contains(stderr.String(), c.want) {
				t.Errorf("exit %d, stderr %q; want 2 and %q", code, stderr.String(), c.want)
			}
		})
	}

	// The origin remote is used when --remote is not given.
	if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", "https://gitlab.com/g/p.git").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	stderr.Reset()
	if code := run([]string{"publish", "--result", writeResult(same)}, &stdout, &stderr); code != protocol.ExitUsage || !strings.Contains(stderr.String(), "GITLAB_TOKEN") {
		t.Errorf("origin not used: exit %d, %q", code, stderr.String())
	}
}
