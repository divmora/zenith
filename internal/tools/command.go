package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/divmora/zenith/pkg/llm"
)

// maxCapture bounds how much command output is kept in memory; the model
// sees at most Env.MaxOutput of it (start and end).
const maxCapture = 4 << 20

// RunCommand runs a shell command in the workspace.
func RunCommand() Tool {
	return Tool{
		Def: llm.Tool{
			Name: "run_command",
			Description: "Run a shell command (bash -c) in the workspace root, e.g. to build, run tests or linters, or inspect the project. " +
				"stdin is empty and nothing is interactive. Output (stdout and stderr together) is truncated in the middle when long. " +
				"Default timeout 120 seconds. Do not run git commit, git push or commands that need network credentials: zenith commits your changes itself.",
			InputSchema: schema(map[string]any{
				"command":         str("The command line to run."),
				"timeout_seconds": num("Timeout in seconds (default 120, max 600)."),
			}, "command"),
		},
		Run: func(ctx context.Context, env *Env, input json.RawMessage) (string, error) {
			var in struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout_seconds"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			if strings.TrimSpace(in.Command) == "" {
				return "", errors.New("command is empty")
			}
			timeout := env.CommandTimeout
			if in.Timeout > 0 {
				timeout = time.Duration(in.Timeout) * time.Second
			}
			timeout = min(timeout, env.MaxCommandTimeout)
			return runShell(ctx, env, in.Command, timeout)
		},
	}
}

func runShell(ctx context.Context, env *Env, command string, timeout time.Duration) (string, error) {
	res, err := Exec(ctx, env, command, timeout)
	if err != nil {
		return "", err
	}
	if res.TimedOut {
		return "", fmt.Errorf("command timed out after %s; output so far:\n%s", timeout, res.Output)
	}
	// A non-zero exit is a normal result (failing tests), not a tool error.
	return fmt.Sprintf("exit code: %d\n%s", res.ExitCode, res.Output), nil
}

// ShellResult is the outcome of Exec.
type ShellResult struct {
	Output   string // stdout and stderr together, start and end kept when long
	ExitCode int    // -1 when the command timed out
	TimedOut bool
	Duration time.Duration
}

// Exec runs a shell command in the workspace with the tools' environment
// and limits: bash -c, scrubbed env, empty stdin, its own process group
// killed on timeout. The error is only for commands that could not run or a
// cancelled parent context; a timeout is reported in the result.
func Exec(ctx context.Context, env *Env, command string, timeout time.Duration) (ShellResult, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	shell := "bash"
	if _, err := exec.LookPath(shell); err != nil {
		shell = "sh"
	}
	cmd := exec.CommandContext(cctx, shell, "-c", command)
	cmd.Dir = env.WS.Root
	cmd.Env = env.CommandEnv
	out := &capped{max: maxCapture}
	cmd.Stdout = out
	cmd.Stderr = out
	setProcessGroup(cmd) // so a timeout kills child processes too
	cmd.WaitDelay = 5 * time.Second

	start := time.Now()
	err := cmd.Run()
	res := ShellResult{Output: out.String(), Duration: time.Since(start)}
	if res.Output == "" {
		res.Output = "(no output)"
	}
	switch {
	case ctx.Err() != nil:
		return res, ctx.Err()
	case cctx.Err() == context.DeadlineExceeded:
		res.TimedOut, res.ExitCode = true, -1
		return res, nil
	}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return res, fmt.Errorf("could not run command: %w", err)
		}
		res.ExitCode = ee.ExitCode()
	}
	return res, nil
}

// capped is a concurrency-safe writer that keeps the first and last max/2
// bytes of everything written.
type capped struct {
	mu      sync.Mutex
	max     int
	head    []byte
	tail    []byte
	dropped int
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	half := c.max / 2
	if room := half - len(c.head); room > 0 {
		k := min(room, len(p))
		c.head = append(c.head, p[:k]...)
		p = p[k:]
	}
	if len(p) > 0 {
		c.tail = append(c.tail, p...)
		if over := len(c.tail) - half; over > 0 {
			c.dropped += over
			c.tail = append(c.tail[:0], c.tail[over:]...)
		}
	}
	return n, nil
}

func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropped == 0 {
		return string(c.head) + string(c.tail)
	}
	return string(c.head) + fmt.Sprintf("\n\n[... %d bytes dropped ...]\n\n", c.dropped) + string(c.tail)
}
