// Package gitx runs git for zenith itself (not for the model).
//
// The model can run arbitrary commands, so it can change .git/config and
// .gitattributes. Git executes some settings (fsmonitor, hooks, filter
// drivers, external diff, pagers), so every call here turns those off and
// runs with the same scrubbed environment as the model's commands: even if
// something slips through, it gets no secrets.
package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/divmora/zenith/internal/policy"
)

// safeConfig disables settings that make git run other programs.
var safeConfig = []string{
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.pager=cat",
	"-c", "core.sshCommand=false",
	"-c", "diff.external=",
	"-c", "protocol.allow=never",
	"-c", "credential.helper=",
	"-c", "commit.gpgSign=false",
	"-c", "tag.gpgSign=false",
	"-c", "safe.directory=*",
}

// Git runs git in one repository.
type Git struct {
	Dir string
}

// Run runs git with args and returns trimmed stdout. The error includes stderr.
func (g Git) Run(ctx context.Context, args ...string) (string, error) {
	out, err := g.RunRaw(ctx, args...)
	return strings.TrimSpace(out), err
}

// RunRaw is Run without trimming (for -z output).
func (g Git) RunRaw(ctx context.Context, args ...string) (string, error) {
	full := append(append([]string{"-C", g.Dir}, safeConfig...), args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(policy.CommandEnv(nil), "GIT_CONFIG_NOSYSTEM=1", "GIT_ATTR_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// IsRepo reports whether Dir is inside a git work tree.
func (g Git) IsRepo(ctx context.Context) bool {
	out, err := g.Run(ctx, "rev-parse", "--is-inside-work-tree")
	return err == nil && out == "true"
}

// Head returns the current commit SHA.
func (g Git) Head(ctx context.Context) (string, error) { return g.Run(ctx, "rev-parse", "HEAD") }

// CurrentBranch returns the branch name, or "" when HEAD is detached.
func (g Git) CurrentBranch(ctx context.Context) string {
	out, err := g.Run(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return out
}

// Dirty reports whether the work tree has uncommitted or untracked changes.
func (g Git) Dirty(ctx context.Context) (bool, error) {
	out, err := g.Run(ctx, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// PushOptions configures Push.
type PushOptions struct {
	// Header is an HTTP header sent only to URL's scheme and host, such as
	// "Authorization: Basic ...". It goes to git through environment
	// config, so it never appears in a process list.
	Header string
	// Protocol is the one transport allowed (default "https"); tests use
	// "file".
	Protocol string
}

// Push pushes sha to refs/heads/branch at url. Hooks stay off and only
// the one transport is allowed; redirects are not followed, so the header
// cannot be sent anywhere else.
func (g Git) Push(ctx context.Context, url, sha, branch string, o PushOptions) (string, error) {
	proto := o.Protocol
	if proto == "" {
		proto = "https"
	}
	args := append(append([]string{"-C", g.Dir}, safeConfig...),
		"-c", "protocol."+proto+".allow=always",
		"-c", "http.followRedirects=false",
		"-c", "push.recurseSubmodules=no",
		"push", "--porcelain", "--no-verify", url, sha+":refs/heads/"+branch)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(policy.CommandEnv(nil), "GIT_CONFIG_NOSYSTEM=1", "GIT_ATTR_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0")
	if o.Header != "" {
		key := "http.extraHeader"
		if scheme, rest, ok := strings.Cut(url, "://"); ok {
			host, _, _ := strings.Cut(rest, "/")
			key = "http." + scheme + "://" + host + "/.extraHeader"
		}
		cmd.Env = append(cmd.Env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0="+key, "GIT_CONFIG_VALUE_0="+o.Header)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String() + "\n" + stdout.String())
		if o.Header != "" {
			msg = strings.ReplaceAll(msg, o.Header, "[header]")
		}
		return "", fmt.Errorf("git push %s: %w: %s", branch, err, msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}
