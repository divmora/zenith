package policy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "src"), filepath.Join(root, "inner")); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}

	ok := map[string]string{
		"src/a.go":                         "src/a.go",
		"./src/../src/new/b.go":            "src/new/b.go",
		filepath.Join(ws.Root, "x.txt"):    "x.txt",
		"inner/c.go":                       "src/c.go", // symlink inside the workspace
		"":                                 ".",
		"src/.gitignore":                   "src/.gitignore",
		filepath.Join("does", "not", "ex"): "does/not/ex",
	}
	for in, want := range ok {
		_, rel, err := ws.Resolve(in, true)
		if err != nil || filepath.ToSlash(rel) != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", in, rel, err, want)
		}
	}

	bad := map[string]error{
		"../x":                 ErrOutside,
		"/etc/passwd":          ErrOutside,
		"escape/secret":        ErrOutside, // symlink out of the workspace
		"escape/new/file":      ErrOutside,
		"src/../../x":          ErrOutside,
		".git/config":          ErrGitDir,
		"src/.git/hooks/pre":   ErrGitDir,
		"inner/.git/something": ErrGitDir,
	}
	for in, want := range bad {
		if _, _, err := ws.Resolve(in, true); !errors.Is(err, want) {
			t.Errorf("Resolve(%q, write) err = %v, want %v", in, err, want)
		}
	}
	if _, _, err := ws.Resolve(".git/config", false); err != nil {
		t.Errorf("reading .git should be allowed: %v", err)
	}
}

func TestCommandEnv(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("ANTHROPIC_API_KEY", "secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("GITHUB_TOKEN", "secret")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent")
	t.Setenv("MY_VAR", "ok")
	env := strings.Join(CommandEnv([]string{"MY_VAR"}), "\n")
	for _, want := range []string{"PATH=/usr/bin", "MY_VAR=ok", "CI=true", "GIT_TERMINAL_PROMPT=0"} {
		if !strings.Contains(env, want) {
			t.Errorf("env missing %s", want)
		}
	}
	if strings.Contains(env, "secret") || strings.Contains(env, "SSH_AUTH_SOCK") {
		t.Errorf("secrets leaked into command env:\n%s", env)
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("short", 100); got != "short" {
		t.Errorf("got %q", got)
	}
	s := strings.Repeat("a", 500) + "ERROR at the end"
	got := Truncate(s, 100)
	if len(got) > 160 || !strings.HasSuffix(got, "ERROR at the end") || !strings.Contains(got, "bytes truncated") {
		t.Errorf("got %q", got)
	}
	u := strings.Repeat("é", 100) // 2 bytes each
	if got := Truncate(u, 51); !strings.HasPrefix(got, "é") || strings.ContainsRune(got, '�') {
		t.Errorf("split a UTF-8 sequence: %q", got)
	}
}
