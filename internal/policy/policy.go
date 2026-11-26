// Package policy holds the safety rules the agent's tools run under:
// workspace confinement, the environment commands get, and output caps.
//
// The repository the agent works on is untrusted, and so is everything the
// model asks for. These rules are a second line of defence; the sandbox
// (container, VM) is the first.
package policy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Workspace confines file access to one directory tree.
type Workspace struct {
	Root string // absolute, symlinks resolved
}

// NewWorkspace resolves dir and checks that it is a directory.
func NewWorkspace(dir string) (*Workspace, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace %s: %w", dir, err)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("workspace %s is not a directory", dir)
	}
	return &Workspace{Root: real}, nil
}

// ErrOutside means a path resolves outside the workspace.
var ErrOutside = errors.New("path is outside the workspace")

// ErrGitDir means a write targets the .git directory.
var ErrGitDir = errors.New("the .git directory is managed by zenith and cannot be written")

// Resolve turns a path from the model (relative to the root, or absolute
// inside it) into an absolute path, following symlinks, and checks it stays
// inside the workspace. For writes it also refuses anything under .git.
// It returns the absolute path and the path relative to the root.
func (w *Workspace) Resolve(p string, write bool) (abs, rel string, err error) {
	if strings.ContainsRune(p, 0) {
		return "", "", errors.New("path contains a NUL byte")
	}
	if p == "" {
		p = "."
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(w.Root, p)
	}
	p = filepath.Clean(p)
	real, err := evalExisting(p)
	if err != nil {
		return "", "", err
	}
	rel, err = filepath.Rel(w.Root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", ErrOutside
	}
	if write && InGitDir(rel) {
		return "", "", ErrGitDir
	}
	return real, rel, nil
}

// InGitDir reports whether a workspace-relative path is .git or inside it.
func InGitDir(rel string) bool {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == ".git" {
			return true
		}
	}
	return false
}

// evalExisting resolves symlinks in the longest existing prefix of p and
// appends the rest, so paths to files that don't exist yet still resolve
// through a symlinked parent directory.
func evalExisting(p string) (string, error) {
	var rest []string
	cur := p
	for {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				real = filepath.Join(real, rest[i])
			}
			return real, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, nil
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}

// passEnv is the allowlist of environment variables commands receive.
// Everything else (API keys, cloud credentials, git tokens, SSH agent
// sockets) is dropped, so a prompt-injected command can't read them.
var passEnv = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "TMPDIR", "TZ",
	"LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "LC_MESSAGES",
	// Toolchains.
	"GOPATH", "GOROOT", "GOCACHE", "GOMODCACHE", "GOPROXY", "GOFLAGS", "GOTOOLCHAIN", "CGO_ENABLED",
	"NODE_PATH", "NPM_CONFIG_CACHE", "PNPM_HOME", "COREPACK_HOME", "NVM_DIR",
	"PYTHONPATH", "VIRTUAL_ENV", "PIP_CACHE_DIR", "UV_CACHE_DIR",
	"JAVA_HOME", "MAVEN_OPTS", "GRADLE_USER_HOME",
	"CARGO_HOME", "RUSTUP_HOME",
	"COMPOSER_HOME",
}

// CommandEnv returns the environment for commands the model runs: the
// allowlist above, plus extra names the operator chose to pass, plus fixed
// settings that keep tools non-interactive.
func CommandEnv(extra []string) []string {
	names := append(append([]string(nil), passEnv...), extra...)
	seen := map[string]bool{}
	var env []string
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		if v, ok := os.LookupEnv(n); ok {
			env = append(env, n+"="+v)
		}
	}
	return append(env,
		"CI=true",
		"ZENITH=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"PAGER=cat",
		"NO_COLOR=1",
	)
}

// Truncate keeps the start and end of s when it is longer than max bytes,
// with a marker saying how much was cut. Command output usually has the
// interesting part (the error) at the end, so the tail is kept too.
func Truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	head := max * 6 / 10
	tail := max - head
	// Don't split UTF-8 sequences.
	for head > 0 && !utf8Start(s[head]) {
		head--
	}
	start := len(s) - tail
	for start < len(s) && !utf8Start(s[start]) {
		start++
	}
	return s[:head] + fmt.Sprintf("\n\n[... %d bytes truncated ...]\n\n", start-head) + s[start:]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
