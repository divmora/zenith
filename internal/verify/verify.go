// Package verify finds and runs a project's own checks (its test command),
// so zenith can confirm the agent's work instead of trusting the model's
// claim that tests pass.
package verify

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/divmora/zenith/internal/policy"
	"github.com/divmora/zenith/internal/tools"
	"github.com/divmora/zenith/pkg/protocol"
)

var makeTest = regexp.MustCompile(`(?m)^test\s*:`)

// Detect returns the check commands for the project at root, from its own
// conventions. A Makefile "test" target wins, since projects use it to wrap
// their real setup; otherwise each recognised ecosystem at the root adds its
// standard test command. Nil means nothing was recognised.
func Detect(root string) []string {
	for _, name := range []string{"GNUmakefile", "makefile", "Makefile"} {
		if data, err := os.ReadFile(filepath.Join(root, name)); err == nil && makeTest.Match(data) {
			return []string{"make test"}
		}
	}
	var cmds []string
	if exists(root, "go.mod") {
		cmds = append(cmds, "go vet ./... && go test ./...")
	}
	if cmd := nodeTest(root); cmd != "" {
		cmds = append(cmds, cmd)
	}
	if exists(root, "Cargo.toml") {
		cmds = append(cmds, "cargo test")
	}
	if pythonTests(root) {
		cmds = append(cmds, "python -m pytest -q")
	}
	if cmd := composerTest(root); cmd != "" {
		cmds = append(cmds, cmd)
	}
	return cmds
}

func nodeTest(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return ""
	}
	test := pkg.Scripts["test"]
	if test == "" || strings.Contains(test, "no test specified") {
		return ""
	}
	switch {
	case exists(root, "pnpm-lock.yaml"):
		return "pnpm test"
	case exists(root, "yarn.lock"):
		return "yarn test"
	case exists(root, "bun.lockb"), exists(root, "bun.lock"):
		return "bun run test"
	default:
		return "npm test"
	}
}

func pythonTests(root string) bool {
	if exists(root, "pytest.ini") || exists(root, "conftest.py") {
		return true
	}
	for _, f := range []string{"pyproject.toml", "setup.cfg", "tox.ini"} {
		data, err := os.ReadFile(filepath.Join(root, f))
		if err == nil && (strings.Contains(string(data), "[tool.pytest") || strings.Contains(string(data), "[pytest]") || strings.Contains(string(data), "[tool:pytest]")) {
			return true
		}
	}
	return false
}

func composerTest(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "composer.json"))
	if err != nil {
		return ""
	}
	var c struct {
		Scripts map[string]json.RawMessage `json:"scripts"`
	}
	if json.Unmarshal(data, &c) == nil && c.Scripts["test"] != nil {
		return "composer test"
	}
	return ""
}

func exists(root, name string) bool {
	_, err := os.Stat(filepath.Join(root, name))
	return err == nil
}

// Run runs each command in the workspace with the same sandbox rules as the
// model's commands, and returns one Check per command.
func Run(ctx context.Context, env *tools.Env, cmds []string, stage protocol.CheckStage, round int, timeout time.Duration) ([]protocol.Check, error) {
	out := make([]protocol.Check, 0, len(cmds))
	for _, cmd := range cmds {
		res, err := tools.Exec(ctx, env, cmd, timeout)
		if err != nil {
			return out, err
		}
		out = append(out, protocol.Check{
			Command:    cmd,
			Stage:      stage,
			Round:      round,
			ExitCode:   res.ExitCode,
			Passed:     res.ExitCode == 0 && !res.TimedOut,
			TimedOut:   res.TimedOut,
			DurationMS: res.Duration.Milliseconds(),
			Output:     policy.Truncate(res.Output, env.MaxOutput),
		})
	}
	return out, nil
}

// Judge marks final failures that also failed at the baseline as
// pre-existing, and reports whether any failure is new (blocking).
func Judge(baseline, final []protocol.Check) (blocking bool) {
	failedBefore := map[string]bool{}
	for _, c := range baseline {
		if !c.Passed {
			failedBefore[c.Command] = true
		}
	}
	for i := range final {
		if final[i].Passed {
			continue
		}
		if failedBefore[final[i].Command] {
			final[i].PreExisting = true
			continue
		}
		blocking = true
	}
	return blocking
}

// Report renders failing checks for the model.
func Report(checks []protocol.Check) string {
	var b strings.Builder
	for _, c := range checks {
		if c.Passed || c.PreExisting {
			continue
		}
		status := "exit code " + itoa(c.ExitCode)
		if c.TimedOut {
			status = "timed out"
		}
		b.WriteString("$ " + c.Command + "  (" + status + ")\n" + c.Output + "\n\n")
	}
	return strings.TrimSpace(b.String())
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
