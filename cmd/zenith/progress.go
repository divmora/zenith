package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/divmora/zenith/pkg/llm"
	"github.com/divmora/zenith/pkg/protocol"
)

// progress prints a readable log of the run to stderr.
type progress struct {
	resultPath string // where the result JSON goes, for the publish hint
	mu         sync.Mutex
	w          io.Writer
	streamed   bool // text for the current step was already printed as deltas
	midLine    bool
	reasoning  bool
}

func (p *progress) printf(format string, a ...any) {
	if p.midLine {
		_, _ = fmt.Fprintln(p.w)
		p.midLine = false
	}
	_, _ = fmt.Fprintf(p.w, format, a...)
}

// delta prints streamed model text as it arrives.
func (p *progress) delta(e llm.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch e.Type {
	case llm.EventText:
		if p.reasoning {
			_, _ = fmt.Fprintln(p.w)
			p.reasoning = false
		}
		_, _ = io.WriteString(p.w, e.Text)
		p.streamed = true
		p.midLine = !strings.HasSuffix(e.Text, "\n")
	case llm.EventReasoning:
		if !p.reasoning {
			p.printf("  (thinking) ")
			p.reasoning = true
		}
		_, _ = io.WriteString(p.w, oneLine(e.Text))
		p.midLine = true
	}
}

func (p *progress) event(e protocol.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch e.Type {
	case protocol.EventTaskStarted:
		branch := e.Task.Branch
		if branch == "" {
			branch = "(created after the plan is approved)"
		}
		p.printf("zenith: %s\n", e.Task.Task)
		if e.Task.Persona != "" {
			p.printf("  persona   %s\n", e.Task.Persona)
		}
		p.printf("  workspace %s\n", e.Task.Workspace)
		if e.Task.Worktree != "" {
			p.printf("  worktree  %s\n", e.Task.Worktree)
		}
		p.printf("  base      %s\n  branch    %s\n  model     %s\n\n", short(e.Task.BaseSHA), branch, e.Task.Model)
	case protocol.EventModelCallStart:
		p.streamed, p.reasoning = false, false
	case protocol.EventAssistantText:
		if !p.streamed {
			p.printf("%s\n", e.Text)
		}
	case protocol.EventToolCall:
		p.printf("→ %s %s\n", e.Tool.Name, describe(e.Tool.Input))
	case protocol.EventToolResult:
		mark := "✓"
		if e.Tool.IsError {
			mark = "✗"
		}
		p.printf("  %s %s (%dms)\n", mark, oneLine(firstLine(e.Tool.Output)), e.Tool.DurationMS)
	case protocol.EventCheck:
		c := e.Check
		mark := "✓"
		switch {
		case c.Passed:
		case c.PreExisting || c.Stage == protocol.CheckBaseline:
			mark = "!"
		default:
			mark = "✗"
		}
		label := string(c.Stage)
		if c.Stage == protocol.CheckFinal && c.Round > 1 {
			label = fmt.Sprintf("final, after fix %d", c.Round-1)
		}
		status := fmt.Sprintf("exit %d", c.ExitCode)
		if c.TimedOut {
			status = "timed out"
		}
		p.printf("%s check (%s) %s: %s, %dms\n", mark, label, c.Command, status, c.DurationMS)
	case protocol.EventCompacted:
		p.printf("(context compacted: %s)\n", e.Text)
	case protocol.EventWarning:
		p.printf("warning: %s\n", e.Text)
	case protocol.EventResumed:
		p.printf("resumed from checkpoint (%s)\n\n", oneLine(e.Text))
	case protocol.EventPlanProposed:
		p.printf("\n── proposed plan ──\n%s\n───────────────────\n", e.Text)
	case protocol.EventCommitted:
		p.printf("\ncommitted %s on %s: %d file(s)\n", short(e.Commit.SHA), e.Commit.Branch, len(e.Commit.Files))
	case protocol.EventTaskFinished:
		r := e.Result
		p.printf("\n%s after %d step(s), %d tokens (%d cached)\n", r.Status, r.Steps, r.Usage.Total(), r.Usage.CachedInputTokens)
		if r.Error != "" {
			p.printf("  %s\n", r.Error)
		}
		if r.Summary != "" {
			p.printf("\n%s\n", r.Summary)
		}
		if r.Worktree != "" {
			p.printf("\nThe work is in %s (branch %s). Remove it when done:\n  git worktree remove %s\n", r.Worktree, r.Branch, r.Worktree)
		}
		if r.Status == protocol.StatusDone && r.CommitSHA != "" && p.resultPath != "" {
			p.printf("\nTo open a pull request:\n  zenith publish --result %s\n", p.resultPath)
		}
		if r.Status == protocol.StatusAwaitingApproval {
			p.printf("\nCheckpoint saved to %s. Next:\n  zenith resume --checkpoint %s --approve\n  zenith resume --checkpoint %s --feedback \"...\"\n", r.Checkpoint, r.Checkpoint, r.Checkpoint)
		}
	}
}

// describe renders tool input compactly: string fields, longest last.
func describe(input json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return oneLine(string(input))
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		switch k {
		case "content", "new_string", "old_string", "summary":
			s, _ := m[k].(string)
			parts = append(parts, fmt.Sprintf("%s=<%d bytes>", k, len(s)))
		default:
			parts = append(parts, fmt.Sprintf("%s=%v", k, m[k]))
		}
	}
	return oneLine(strings.Join(parts, " "))
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return s
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
