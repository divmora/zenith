package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/divmora/zenith/internal/policy"
	"github.com/divmora/zenith/internal/tools"
	"github.com/divmora/zenith/pkg/protocol"
)

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755)
		if err := os.WriteFile(filepath.Join(root, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"make test wins", map[string]string{"Makefile": "build:\n\tgo build\n\ntest:\n\tgo test ./...\n", "go.mod": "module x"}, "make test"},
		{"makefile without test", map[string]string{"Makefile": "build:\n\tgo build\n", "go.mod": "module x"}, "go vet ./... && go test ./..."},
		{"npm", map[string]string{"package.json": `{"scripts":{"test":"vitest run"}}`}, "npm test"},
		{"npm placeholder", map[string]string{"package.json": `{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`}, ""},
		{"pnpm", map[string]string{"package.json": `{"scripts":{"test":"jest"}}`, "pnpm-lock.yaml": ""}, "pnpm test"},
		{"go and node", map[string]string{"go.mod": "module x", "package.json": `{"scripts":{"test":"jest"}}`, "yarn.lock": ""}, "go vet ./... && go test ./...|yarn test"},
		{"cargo", map[string]string{"Cargo.toml": "[package]"}, "cargo test"},
		{"pytest", map[string]string{"pyproject.toml": "[tool.pytest.ini_options]\n"}, "python -m pytest -q"},
		{"plain python", map[string]string{"pyproject.toml": "[project]\nname='x'\n"}, ""},
		{"composer", map[string]string{"composer.json": `{"scripts":{"test":"phpunit"}}`}, "composer test"},
		{"nothing", map[string]string{"README.md": "hi"}, ""},
	}
	for _, c := range cases {
		root := t.TempDir()
		write(t, root, c.files)
		if got := strings.Join(Detect(root), "|"); got != c.want {
			t.Errorf("%s: Detect = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRunAndJudge(t *testing.T) {
	root := t.TempDir()
	ws, err := policy.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-secret")
	env := tools.DefaultEnv(ws, policy.CommandEnv(nil))
	checks, err := Run(context.Background(), env, []string{"echo ok", "echo broken >&2; exit 2", "env", "sleep 5"}, protocol.CheckFinal, 1, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !checks[0].Passed || checks[1].Passed || checks[1].ExitCode != 2 || !strings.Contains(checks[1].Output, "broken") {
		t.Errorf("checks = %+v", checks[:2])
	}
	if strings.Contains(checks[2].Output, "sk-secret") {
		t.Error("checks must run with the scrubbed environment")
	}
	if checks[3].Passed || !checks[3].TimedOut {
		t.Errorf("timeout check = %+v", checks[3])
	}

	baseline := []protocol.Check{{Command: "echo broken >&2; exit 2", Passed: false}, {Command: "echo ok", Passed: true}}
	if blocking := Judge(baseline, checks); !blocking {
		t.Error("the new timeout failure should block")
	}
	if !checks[1].PreExisting || checks[3].PreExisting {
		t.Errorf("pre-existing marks wrong: %+v", checks)
	}
	report := Report(checks)
	if strings.Contains(report, "broken") || !strings.Contains(report, "$ sleep 5  (timed out)") {
		t.Errorf("report should list only new failures:\n%s", report)
	}
	if Judge(nil, []protocol.Check{{Command: "x", Passed: true}}) {
		t.Error("all passing must not block")
	}
}
