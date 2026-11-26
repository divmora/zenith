package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/divmora/zenith/internal/policy"
	"github.com/divmora/zenith/internal/tools"
	"github.com/divmora/zenith/pkg/llm"
	"github.com/divmora/zenith/pkg/llm/llmtest"
	"github.com/divmora/zenith/pkg/protocol"
)

func propose(plan string) llmtest.Step {
	s := llmtest.Call("p", "propose_plan", map[string]any{"plan": plan})
	s.Response.Usage = llm.Usage{InputTokens: 100, OutputTokens: 10}
	return s
}

// planRun runs the plan phase and returns the result and checkpoint path.
func planRun(t *testing.T, dir string, steps ...llmtest.Step) (*protocol.Result, string, []protocol.Event) {
	t.Helper()
	var events []protocol.Event
	cfg := config(t, dir, llmtest.New(steps...), &events)
	cfg.PlanFirst = true
	cfg.ModelSpec = "fake:planner"
	cfg.CheckpointPath = filepath.Join(t.TempDir(), "cp.json")
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return res, cfg.CheckpointPath, events
}

func TestPlanFirstStopsForApproval(t *testing.T) {
	dir := repo(t)
	model := llmtest.New(
		llmtest.Call("1", "read_file", map[string]any{"path": "README.md"}),
		// Tries to write while planning: must be refused.
		llmtest.Call("2", "write_file", map[string]any{"path": "sneaky.txt", "content": "x"}),
		propose("1. Add calc/calc.go\n2. Test it"),
	)
	var events []protocol.Event
	cfg := config(t, dir, model, &events)
	cfg.PlanFirst = true
	cfg.ModelSpec = "fake:planner"
	cfg.CheckpointPath = filepath.Join(t.TempDir(), "cp.json")
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != protocol.StatusAwaitingApproval || res.Status.ExitCode() != protocol.ExitAwaitingApproval {
		t.Fatalf("result = %+v", res)
	}
	if res.Plan != "1. Add calc/calc.go\n2. Test it" || res.Checkpoint != cfg.CheckpointPath || res.CommitSHA != "" || res.Branch != "" {
		t.Errorf("result = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "sneaky.txt")); err == nil {
		t.Error("a file was written during planning")
	}
	if got := gitRun(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("planning must not create or switch branches, on %q", got)
	}
	if gitRun(t, dir, "status", "--porcelain") != "" {
		t.Error("planning changed the work tree")
	}
	for _, d := range model.Requests()[0].Tools {
		if tool, _ := tools.Planning().Get(d.Name); !tool.ReadOnly {
			t.Errorf("plan request offered %s", d.Name)
		}
	}
	if !strings.Contains(model.Requests()[0].Messages[0].Text, "Planning phase") {
		t.Errorf("first message should explain the planning phase")
	}

	fi, err := os.Stat(cfg.CheckpointPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("checkpoint mode = %v, want 0600", fi.Mode().Perm())
	}
	cp, err := LoadCheckpoint(cfg.CheckpointPath)
	if err != nil {
		t.Fatal(err)
	}
	if cp.V != protocol.CheckpointVersion || cp.Model != "fake:planner" || cp.BaseSHA != res.BaseSHA || cp.Rounds != 1 ||
		!strings.HasPrefix(cp.Branch, "zenith/") || cp.Usage.Total() != 110 {
		t.Errorf("checkpoint = %+v", cp)
	}
	// user task, then (assistant, tool results) x 3
	if n := len(cp.Messages); n != 7 || len(cp.Messages[6].ToolResults) != 1 {
		t.Errorf("checkpoint conversation has %d messages", n)
	}
	var sawPlan bool
	for _, e := range events {
		if e.Phase != protocol.PhasePlan {
			t.Errorf("%s event in phase %q", e.Type, e.Phase)
		}
		if e.Type == protocol.EventPlanProposed {
			sawPlan = true
		}
	}
	if !sawPlan {
		t.Error("no plan_proposed event")
	}
}

func TestResumeApprovedInFreshClone(t *testing.T) {
	dir := repo(t)
	plan, cpPath, _ := planRun(t, dir, propose("Add calc.go"))
	cp, err := LoadCheckpoint(cpPath)
	if err != nil {
		t.Fatal(err)
	}

	// A fresh sandbox: a new clone at the same commit.
	clone := filepath.Join(t.TempDir(), "clone")
	gitRun(t, dir, "clone", "-q", dir, clone)

	exec := llmtest.New(
		llmtest.Step{Reply: func(req *llm.Request) (*llm.Response, error) {
			msgs := req.Messages
			if last := msgs[len(msgs)-1]; !strings.Contains(last.Text, "plan is approved") {
				return nil, errors.New("last message is not the approval")
			}
			if len(msgs) != len(cp.Messages)+1 {
				return nil, errors.New("planning conversation was not carried over")
			}
			for _, d := range req.Tools {
				if d.Name == "write_file" {
					return &llm.Response{Message: llm.Message{ToolCalls: []llm.ToolCall{
						{ID: "w", Name: "write_file", Input: []byte(`{"path":"calc.go","content":"package calc\n"}`)},
					}}, Usage: llm.Usage{InputTokens: 50, OutputTokens: 5}}, nil
				}
			}
			return nil, errors.New("execute phase has no write_file")
		}},
		finish("Implemented the plan.", true),
	)
	var events []protocol.Event
	ws, err := policy.NewWorkspace(clone)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config(t, clone, exec, &events)
	cfg.ToolEnv = tools.DefaultEnv(ws, policy.CommandEnv(nil))
	cfg.Resume = cp
	cfg.Approve = true
	cfg.ApproveNotes = "Name the file calc.go."
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if last := exec.Requests()[0].Messages; !strings.Contains(last[len(last)-1].Text, "<notes>\nName the file calc.go.\n</notes>") {
		t.Errorf("approval notes not sent")
	}
	if res.Status != protocol.StatusDone || res.Branch != cp.Branch || res.CommitSHA == "" || res.Plan != plan.Plan {
		t.Fatalf("result = %+v", res)
	}
	if res.Usage.Total() != cp.Usage.Total()+55 || res.Steps != 2 {
		t.Errorf("usage should include planning: %+v steps=%d", res.Usage, res.Steps)
	}
	if res.Files[0] != "calc.go" || gitRun(t, clone, "rev-parse", "--abbrev-ref", "HEAD") != cp.Branch {
		t.Errorf("commit/branch wrong: %+v", res)
	}
	if events[1].Type != protocol.EventResumed || events[1].Text != "approved with notes: Name the file calc.go." || events[1].Phase != protocol.PhaseExecute {
		t.Errorf("second event = %+v", events[1])
	}
}

func TestResumeWithFeedbackRevisesPlan(t *testing.T) {
	dir := repo(t)
	_, cpPath, _ := planRun(t, dir, propose("v1: rewrite everything"))
	cp, _ := LoadCheckpoint(cpPath)

	model := llmtest.New(llmtest.Step{Reply: func(req *llm.Request) (*llm.Response, error) {
		last := req.Messages[len(req.Messages)-1]
		if !strings.Contains(last.Text, "<feedback>\nSmaller, please.\n</feedback>") {
			return nil, errors.New("feedback missing")
		}
		for _, d := range req.Tools {
			if d.Name == "write_file" || d.Name == "run_command" {
				return nil, errors.New("feedback round must stay read-only")
			}
		}
		return &llm.Response{Message: llm.Message{ToolCalls: []llm.ToolCall{
			{ID: "p2", Name: "propose_plan", Input: []byte(`{"plan":"v2: one small change"}`)},
		}}}, nil
	}})
	var events []protocol.Event
	cfg := config(t, dir, model, &events)
	cfg.Resume = cp
	cfg.Feedback = "Smaller, please."
	cfg.CheckpointPath = cpPath
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != protocol.StatusAwaitingApproval || res.Plan != "v2: one small change" {
		t.Fatalf("result = %+v", res)
	}
	cp2, _ := LoadCheckpoint(cpPath)
	if cp2.Rounds != 2 || cp2.Plan != "v2: one small change" || cp2.Branch != cp.Branch || len(cp2.Messages) != len(cp.Messages)+3 {
		t.Errorf("updated checkpoint = rounds %d plan %q msgs %d", cp2.Rounds, cp2.Plan, len(cp2.Messages))
	}
	if gitRun(t, dir, "rev-parse", "--abbrev-ref", "HEAD") != "main" {
		t.Error("feedback round must not create the branch")
	}
}

func TestPlanFromProseAfterNudges(t *testing.T) {
	res, _, _ := planRun(t, repo(t), llmtest.Text("Plan: a"), llmtest.Text("Plan: b"), llmtest.Text("Plan: edit x.go"))
	if res.Status != protocol.StatusAwaitingApproval || res.Plan != "Plan: edit x.go" {
		t.Fatalf("result = %+v", res)
	}
}

func TestResumeSetupErrors(t *testing.T) {
	dir := repo(t)
	_, cpPath, _ := planRun(t, dir, propose("plan"))
	cp, _ := LoadCheckpoint(cpPath)
	var se *SetupError
	var events []protocol.Event

	cases := map[string]func(*Config){
		"neither":       func(c *Config) {},
		"both":          func(c *Config) { c.Approve = true; c.Feedback = "x" },
		"no checkpoint": func(c *Config) { c.Feedback = "x"; c.CheckpointPath = "" },
		"notes w/o ok":  func(c *Config) { c.Feedback = "x"; c.ApproveNotes = "n" },
		"bad version":   func(c *Config) { c.Approve = true; bad := *cp; bad.V = 99; c.Resume = &bad },
	}
	for name, mod := range cases {
		cfg := config(t, dir, llmtest.New(), &events)
		cfg.Resume = cp
		cfg.CheckpointPath = cpPath
		mod(&cfg)
		if _, err := Run(context.Background(), cfg); !errors.As(err, &se) {
			t.Errorf("%s: err = %v, want SetupError", name, err)
		}
	}

	// The repository moved on since planning.
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "later")
	cfg := config(t, dir, llmtest.New(), &events)
	cfg.Resume = cp
	cfg.Approve = true
	if _, err := Run(context.Background(), cfg); !errors.As(err, &se) || !strings.Contains(err.Error(), "planned at") {
		t.Errorf("moved base: err = %v", err)
	}

	cfg = config(t, dir, llmtest.New(), &events)
	cfg.PlanFirst = true
	if _, err := Run(context.Background(), cfg); !errors.As(err, &se) {
		t.Errorf("plan-first without checkpoint path: %v", err)
	}
}

func TestPersonaCarriedThroughPlanAndResume(t *testing.T) {
	dir := repo(t)
	p := &protocol.Persona{Name: "sentinel", Title: "Security", Instructions: "Fix one security issue."}
	var events []protocol.Event
	model := llmtest.New(propose("Remove the hardcoded key"))
	cfg := config(t, dir, model, &events)
	cfg.Persona = p
	cfg.PlanFirst = true
	cfg.CheckpointPath = filepath.Join(t.TempDir(), "cp.json")
	res, err := Run(context.Background(), cfg)
	if err != nil || res.Persona != "sentinel" {
		t.Fatalf("plan: %+v %v", res, err)
	}
	if !strings.Contains(model.Requests()[0].System, "Fix one security issue.") {
		t.Error("persona instructions not in the system prompt")
	}
	cp, _ := LoadCheckpoint(cfg.CheckpointPath)
	if cp.Persona == nil || cp.Persona.Instructions != p.Instructions {
		t.Fatalf("checkpoint persona = %+v", cp.Persona)
	}

	exec := llmtest.New(
		llmtest.Call("1", "write_file", map[string]any{"path": "fix.txt", "content": "x\n"}),
		finish("Removed it.", true),
	)
	cfg = config(t, dir, exec, &events)
	cfg.Resume = cp
	cfg.Approve = true // no Persona in cfg: it must come from the checkpoint
	res, err = Run(context.Background(), cfg)
	if err != nil || res.Persona != "sentinel" {
		t.Fatalf("resume: %+v %v", res, err)
	}
	if !strings.Contains(exec.Requests()[0].System, "Fix one security issue.") {
		t.Error("resumed run lost the persona")
	}
	if subject := gitRun(t, dir, "log", "-1", "--format=%s"); !strings.HasPrefix(subject, "zenith(sentinel): ") {
		t.Errorf("commit subject = %q", subject)
	}
}
