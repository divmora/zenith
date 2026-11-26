package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/divmora/zenith/internal/prompt"
	"github.com/divmora/zenith/pkg/llm"
	"github.com/divmora/zenith/pkg/llm/llmtest"
	"github.com/divmora/zenith/pkg/protocol"
)

// checkedRepo has a Makefile whose test passes while answer.txt says ok.
func checkedRepo(t *testing.T, answer string) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	files := map[string]string{
		"Makefile":   "test:\n\tgrep -q '^ok$$' answer.txt || (echo 'answer.txt must say ok' >&2; exit 1)\n",
		"answer.txt": answer,
	}
	for p, c := range files {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init")
	return dir
}

func write(path, content string) llmtest.Step {
	return llmtest.Call("w-"+content, "write_file", map[string]any{"path": path, "content": content})
}

func checksOf(events []protocol.Event) []protocol.Check {
	var out []protocol.Check
	for _, e := range events {
		if e.Type == protocol.EventCheck {
			out = append(out, *e.Check)
		}
	}
	return out
}

func TestVerifyFailureGoesBackToTheModel(t *testing.T) {
	dir := checkedRepo(t, "ok\n")
	model := llmtest.New(
		write("answer.txt", "wrong\n"),
		finish("Changed the answer.", true),
		llmtest.Step{Reply: func(req *llm.Request) (*llm.Response, error) {
			last := req.Messages[len(req.Messages)-1]
			if len(last.ToolResults) != 1 || !last.ToolResults[0].IsError || !strings.Contains(last.ToolResults[0].Content, "verification failed") {
				return nil, errors.New("finish result should say verification failed")
			}
			if !strings.Contains(last.Text, "<check_failures>") || !strings.Contains(last.Text, "answer.txt must say ok") || !strings.Contains(last.Text, "attempt 1 of 3") {
				return nil, errors.New("check output missing: " + last.Text)
			}
			return &llm.Response{Message: llm.Message{ToolCalls: []llm.ToolCall{{ID: "fix", Name: "write_file", Input: []byte(`{"path":"answer.txt","content":"ok\n"}`)}}}}, nil
		}},
		finish("Fixed it.", true),
	)
	var events []protocol.Event
	res, err := Run(context.Background(), config(t, dir, model, &events))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != protocol.StatusDone || res.Summary != "Fixed it." {
		t.Fatalf("result = %+v", res)
	}
	checks := checksOf(events)
	if len(checks) != 3 || checks[0].Stage != protocol.CheckBaseline || !checks[0].Passed ||
		checks[1].Passed || checks[1].Round != 1 || !checks[2].Passed || checks[2].Round != 2 {
		t.Errorf("checks = %+v", checks)
	}
	if len(res.Checks) != 2 || !res.Checks[1].Passed {
		t.Errorf("result checks should be baseline + last round: %+v", res.Checks)
	}
	// The fix restored the original content, so there is nothing to commit.
	if res.CommitSHA != "" || len(res.Files) != 0 {
		t.Errorf("unexpected commit: %+v", res)
	}
}

func TestVerifyGivesUpAfterMaxAttempts(t *testing.T) {
	dir := checkedRepo(t, "ok\n")
	model := llmtest.New(write("answer.txt", "wrong\n"), finish("done", true), finish("done again", true))
	var events []protocol.Event
	cfg := config(t, dir, model, &events)
	cfg.MaxFixAttempts = 1
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != protocol.StatusFailed || !strings.Contains(res.Error, "verification failed after 1 fix attempt(s): make test") {
		t.Fatalf("result = %+v", res)
	}
	if !strings.HasPrefix(gitRun(t, dir, "log", "-1", "--format=%s"), "zenith (incomplete)") {
		t.Error("failed verification should commit as incomplete")
	}
	if model.Remaining() != 0 {
		t.Errorf("%d steps unused", model.Remaining())
	}
}

func TestVerifyPreExistingFailureDoesNotBlock(t *testing.T) {
	dir := checkedRepo(t, "broken\n") // make test already fails
	model := llmtest.New(
		llmtest.Step{Reply: func(req *llm.Request) (*llm.Response, error) {
			if !strings.Contains(req.Messages[0].Text, "<baseline_failures>") {
				return nil, errors.New("the model was not told about baseline failures")
			}
			return &llm.Response{Message: llm.Message{ToolCalls: []llm.ToolCall{{ID: "1", Name: "write_file", Input: []byte(`{"path":"docs.md","content":"docs\n"}`)}}}}, nil
		}},
		finish("Added docs.", true),
	)
	var events []protocol.Event
	res, err := Run(context.Background(), config(t, dir, model, &events))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != protocol.StatusDone || len(res.Checks) != 2 || !res.Checks[1].PreExisting {
		t.Fatalf("result = %+v", res)
	}
	var warned bool
	for _, e := range events {
		if e.Type == protocol.EventWarning && strings.Contains(e.Text, "already failed") {
			warned = true
		}
	}
	if !warned {
		t.Error("no warning about the pre-existing failure")
	}
}

func TestVerifyOptions(t *testing.T) {
	t.Run("no verify", func(t *testing.T) {
		var events []protocol.Event
		cfg := config(t, checkedRepo(t, "ok\n"), llmtest.New(write("answer.txt", "wrong\n"), finish("x", true)), &events)
		cfg.NoVerify = true
		res, _ := Run(context.Background(), cfg)
		if res.Status != protocol.StatusDone || len(checksOf(events)) != 0 {
			t.Errorf("result = %+v, checks %d", res, len(checksOf(events)))
		}
	})
	t.Run("explicit commands", func(t *testing.T) {
		var events []protocol.Event
		cfg := config(t, checkedRepo(t, "ok\n"), llmtest.New(finish("x", true)), &events)
		cfg.Verify = []string{"test -f answer.txt", "true"}
		res, _ := Run(context.Background(), cfg)
		checks := checksOf(events)
		if res.Status != protocol.StatusDone || len(checks) != 4 || checks[0].Command != "test -f answer.txt" {
			t.Errorf("checks = %+v", checks)
		}
	})
	t.Run("check timeout", func(t *testing.T) {
		var events []protocol.Event
		// Fast at the baseline; hangs once the model creates "slow".
		cfg := config(t, checkedRepo(t, "ok\n"), llmtest.New(write("slow", "x"), finish("x", true)), &events)
		cfg.Verify = []string{"if [ -f slow ]; then sleep 5; fi"}
		cfg.CheckTimeout = 300 * time.Millisecond
		cfg.MaxFixAttempts = -1
		start := time.Now()
		res, _ := Run(context.Background(), cfg)
		c := res.Checks[len(res.Checks)-1]
		if res.Status != protocol.StatusFailed || !c.TimedOut || c.PreExisting || !res.Checks[0].Passed || time.Since(start) > 4*time.Second {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("prose finish is verified too", func(t *testing.T) {
		var events []protocol.Event
		model := llmtest.New(write("answer.txt", "wrong\n"), llmtest.Text("done"), llmtest.Text("done"), llmtest.Text("done"))
		cfg := config(t, checkedRepo(t, "ok\n"), model, &events)
		cfg.MaxFixAttempts = -1
		res, _ := Run(context.Background(), cfg)
		if res.Status != protocol.StatusFailed || !strings.Contains(res.Error, "verification failed") {
			t.Errorf("result = %+v", res)
		}
	})
}

// alternates checks the history is valid for every provider: user first,
// roles alternate, and each tool result answers a call in the message before.
func alternates(t *testing.T, msgs []llm.Message) {
	t.Helper()
	req := llm.Request{Messages: msgs}
	if err := req.Validate(); err != nil {
		t.Fatalf("invalid history: %v", err)
	}
	if msgs[0].Role != llm.User {
		t.Fatalf("history starts with %s", msgs[0].Role)
	}
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role == msgs[i-1].Role {
			t.Fatalf("messages %d and %d are both %s", i-1, i, msgs[i].Role)
		}
		for _, tr := range msgs[i].ToolResults {
			found := false
			for _, c := range msgs[i-1].ToolCalls {
				found = found || c.ID == tr.CallID
			}
			if !found {
				t.Fatalf("message %d answers call %s that message %d did not make", i, tr.CallID, i-1)
			}
		}
	}
}

func bigRepo(t *testing.T) string {
	dir := repo(t)
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(strings.Repeat("line of content\n", 400)), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "big")
	return dir
}

func readBig(id string) llmtest.Step {
	return llmtest.Call(id, "read_file", map[string]any{"path": "big.txt"})
}

func TestCompactionElidesOldOutputs(t *testing.T) {
	steps := []llmtest.Step{}
	for i := 0; i < 8; i++ {
		steps = append(steps, readBig(string(rune('a'+i))))
	}
	steps = append(steps, finish("read a lot", true))
	model := llmtest.New(steps...)
	var events []protocol.Event
	cfg := config(t, bigRepo(t), model, &events)
	cfg.ContextTokens = 12_000 // ~9k-token threshold: each big read is ~3k tokens
	res, err := Run(context.Background(), cfg)
	if err != nil || res.Status != protocol.StatusDone {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if res.Compactions == 0 {
		t.Fatal("expected compaction")
	}
	last := model.Requests()[len(model.Requests())-1]
	alternates(t, last.Messages)
	elided, full := 0, 0
	for _, m := range last.Messages {
		for _, tr := range m.ToolResults {
			if strings.HasSuffix(tr.Content, elidedNote) {
				elided++
			} else if len(tr.Content) > 5000 {
				full++
			}
		}
	}
	if elided == 0 || full == 0 {
		t.Errorf("want old outputs elided and recent ones kept: elided=%d full=%d", elided, full)
	}
	for _, m := range last.Messages[len(last.Messages)-keepRecent:] {
		for _, tr := range m.ToolResults {
			if strings.HasSuffix(tr.Content, elidedNote) {
				t.Error("a recent tool output was elided")
			}
		}
	}
}

func TestCompactionSummarizes(t *testing.T) {
	var steps []llmtest.Step
	for i := 0; i < 8; i++ {
		steps = append(steps, llmtest.Call(string(rune('a'+i)), "list_files", map[string]any{}))
	}
	steps = append(steps, llmtest.Step{Reply: func(req *llm.Request) (*llm.Response, error) {
		first := req.Messages[0].Text
		if !strings.Contains(first, "<progress_so_far>") || !strings.Contains(first, "Listed files; README.md exists.") || !strings.Contains(first, "<task>") {
			return nil, errors.New("summary or task missing from the first message: " + first)
		}
		if req.Messages[1].Role != llm.Assistant {
			return nil, errors.New("kept tail must start with the assistant")
		}
		return &llm.Response{Message: llm.Message{ToolCalls: []llm.ToolCall{{ID: "f", Name: "finish", Input: []byte(`{"summary":"ok","success":true}`)}}}}, nil
	}})
	model := llmtest.New(steps...)
	sum := &summarizer{Client: model}
	var events []protocol.Event
	cfg := config(t, repo(t), sum, &events)
	// list_files results are short, so eliding can't help, and a 1-token
	// window is always exceeded: the summary stage must run.
	cfg.ContextTokens = 1
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.calls == 0 || res.Status != protocol.StatusDone || res.Compactions == 0 {
		t.Fatalf("summaries=%d result=%+v", sum.calls, res)
	}
	if res.Usage.ModelCalls != 9+sum.calls {
		t.Errorf("summary calls should count in usage: %d calls, %d summaries", res.Usage.ModelCalls, sum.calls)
	}
	alternates(t, model.Requests()[len(model.Requests())-1].Messages)
	for _, r := range model.Requests() {
		alternates(t, r.Messages)
	}
}

// summarizer answers compaction summary calls and passes the rest on.
type summarizer struct {
	llm.Client
	calls int
}

func (s *summarizer) Chat(ctx context.Context, req *llm.Request, h llm.EventHandler) (*llm.Response, error) {
	if req.System != prompt.SummarySystem {
		return s.Client.Chat(ctx, req, h)
	}
	if len(req.Tools) != 0 || !strings.Contains(req.Messages[0].Text, "[tool call] list_files") {
		return nil, errors.New("bad summary request")
	}
	s.calls++
	return &llm.Response{Message: llm.Message{Role: llm.Assistant, Text: "- Listed files; README.md exists."}, Usage: llm.Usage{InputTokens: 10, OutputTokens: 10}}, nil
}
