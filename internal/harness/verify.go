package harness

import (
	"context"
	"fmt"
	"strings"

	"github.com/divmora/zenith/internal/prompt"
	"github.com/divmora/zenith/internal/verify"
	"github.com/divmora/zenith/pkg/llm"
	"github.com/divmora/zenith/pkg/protocol"
)

// runBaseline runs the checks before any change, so failures that were
// already there are not blamed on the agent, and tells the model about
// them in its first message. It returns false when the run's time ran out.
func (r *runner) runBaseline(ctx context.Context, req *llm.Request) bool {
	if len(r.checks) == 0 {
		return true
	}
	checks, err := verify.Run(ctx, r.cfg.ToolEnv, r.checks, protocol.CheckBaseline, 0, r.cfg.CheckTimeout)
	r.baseline = checks
	for i := range checks {
		r.emit(protocol.Event{Type: protocol.EventCheck, Check: &checks[i]})
	}
	if err != nil {
		return ctx.Err() == nil
	}
	if report := verify.Report(checks); report != "" {
		last := &req.Messages[len(req.Messages)-1]
		last.Text += prompt.BaselineFailing(report)
	}
	return true
}

// verifyFinished runs the checks after the agent says it is done, and sets
// the final status. When new failures remain and fix attempts are left, it
// returns the message for the model and again=true; the run goes on.
func (r *runner) verifyFinished(ctx, parent context.Context) (report string, again bool) {
	if len(r.checks) == 0 {
		r.result.Status = protocol.StatusDone
		return "", false
	}
	checks, err := verify.Run(ctx, r.cfg.ToolEnv, r.checks, protocol.CheckFinal, r.fixRounds+1, r.cfg.CheckTimeout)
	blocking := verify.Judge(r.baseline, checks)
	for i := range checks {
		r.emit(protocol.Event{Type: protocol.EventCheck, Check: &checks[i]})
	}
	r.result.Checks = append(append([]protocol.Check(nil), r.baseline...), checks...)
	if err != nil {
		r.stopCtx(parent)
		return "", false
	}
	if !blocking {
		r.result.Status = protocol.StatusDone
		var pre []string
		for _, c := range checks {
			if c.PreExisting {
				pre = append(pre, c.Command)
			}
		}
		if len(pre) > 0 {
			r.emit(protocol.Event{Type: protocol.EventWarning, Text: "checks that already failed before the change still fail: " + strings.Join(pre, "; ")})
		}
		return "", false
	}
	if r.fixRounds < r.cfg.MaxFixAttempts {
		r.fixRounds++
		return prompt.VerifyFailed(verify.Report(checks), r.fixRounds, r.cfg.MaxFixAttempts), true
	}
	var failed []string
	for _, c := range checks {
		if !c.Passed && !c.PreExisting {
			failed = append(failed, c.Command)
		}
	}
	r.stop(protocol.StatusFailed, fmt.Sprintf("verification failed after %d fix attempt(s): %s", r.fixRounds, strings.Join(failed, "; ")))
	return "", false
}
