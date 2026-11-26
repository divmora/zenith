// Package publish pushes the branch a run produced and opens a pull
// request (a merge request on GitLab) for it.
//
// It runs after the agent, without a model, and is the only part of
// zenith that holds a code-host token: `zenith run` never gets push
// credentials, so a prompt injection in the repository cannot push or
// open anything. The pull request text is partly written by the model, so
// it goes through Sanitize (no @mentions, no GitLab quick actions).
package publish

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/divmora/zenith/internal/gitx"
	"github.com/divmora/zenith/pkg/protocol"
)

// Options says what to publish and where.
type Options struct {
	Result *protocol.Result
	Dir    string  // a checkout that has Result.CommitSHA
	Remote *Remote // where to push and open the pull request
	Token  string
	Base   string // target branch; default: the run's base branch if the remote has it, else the default branch
	Title  string // default: the commit's subject
	Draft  bool
	// AllowIncomplete publishes a run that did not finish (always as a draft).
	AllowIncomplete bool
	HTTPClient      *http.Client
	// PushProtocol overrides the transport (tests push to file://).
	PushProtocol string
	// Log receives progress lines; nil discards them.
	Log func(string)
}

// Outcome is what publish did. It is written as JSON by `zenith publish --output`.
type Outcome struct {
	V         int    `json:"v"`
	Platform  string `json:"platform"`
	Repo      string `json:"repo"`
	URL       string `json:"url"`
	Number    int    `json:"number"`
	Created   bool   `json:"created"` // false when an open pull request for the branch already existed
	Draft     bool   `json:"draft"`
	Branch    string `json:"branch"`
	Base      string `json:"base"`
	CommitSHA string `json:"commit_sha"`
}

// ErrNotPublishable means the result has nothing (yet) to publish.
var ErrNotPublishable = errors.New("nothing to publish")

// Check reports whether res can be published.
func Check(res *protocol.Result, allowIncomplete bool) error {
	switch {
	case res.Status == protocol.StatusAwaitingApproval:
		return fmt.Errorf("%w: the plan is awaiting approval; run `zenith resume --approve` first", ErrNotPublishable)
	case res.Branch == "" || res.CommitSHA == "":
		return fmt.Errorf("%w: the run made no changes", ErrNotPublishable)
	case res.Status != protocol.StatusDone && !allowIncomplete:
		return fmt.Errorf("%w: the run ended with status %s; pass --allow-incomplete to open a draft anyway", ErrNotPublishable, res.Status)
	}
	return nil
}

// Publish pushes the run's commit to its branch on the remote and opens a
// pull request, or returns the one already open for that branch. It
// pushes Result.CommitSHA, the commit zenith verified, even if the local
// branch has moved since.
func Publish(ctx context.Context, o Options) (*Outcome, error) {
	res := o.Result
	if err := Check(res, o.AllowIncomplete); err != nil {
		return nil, err
	}
	log := o.Log
	if log == nil {
		log = func(string) {}
	}
	draft := o.Draft || res.Status != protocol.StatusDone
	git := gitx.Git{Dir: o.Dir}
	if _, err := git.Run(ctx, "cat-file", "-e", res.CommitSHA+"^{commit}"); err != nil {
		return nil, fmt.Errorf("commit %s is not in %s; point --workspace at the repository the run used", short(res.CommitSHA), o.Dir)
	}
	title := strings.TrimSpace(o.Title)
	if title == "" {
		subject, err := git.Run(ctx, "log", "-1", "--format=%s", res.CommitSHA)
		if err != nil {
			return nil, err
		}
		title = subject
	}

	host, err := NewHost(o.Remote, o.Token, o.HTTPClient)
	if err != nil {
		return nil, err
	}
	base := o.Base
	if base == "" && res.BaseBranch != "" && res.BaseBranch != res.Branch {
		ok, err := host.BranchExists(ctx, res.BaseBranch)
		if err != nil {
			return nil, err
		}
		if ok {
			base = res.BaseBranch
		}
	}
	if base == "" {
		if base, err = host.DefaultBranch(ctx); err != nil {
			return nil, err
		}
		if base == "" {
			return nil, fmt.Errorf("%s has no default branch; pass --base", o.Remote.Path)
		}
	}
	if base == res.Branch {
		return nil, fmt.Errorf("the base branch and the run's branch are both %s", base)
	}

	out := &Outcome{
		V: protocol.Version, Platform: o.Remote.Platform, Repo: o.Remote.Host + "/" + o.Remote.Path,
		Branch: res.Branch, Base: base, CommitSHA: res.CommitSHA,
	}
	if _, err := git.Push(ctx, o.Remote.PushURL, res.CommitSHA, res.Branch, gitx.PushOptions{
		Header: host.PushHeader(), Protocol: o.PushProtocol,
	}); err != nil {
		return nil, err
	}
	log(fmt.Sprintf("pushed %s (%s) to %s", res.Branch, short(res.CommitSHA), out.Repo))

	if pr, err := host.FindOpen(ctx, res.Branch); err != nil {
		return nil, err
	} else if pr != nil {
		out.URL, out.Number, out.Draft = pr.URL, pr.Number, pr.Draft
		log(fmt.Sprintf("a pull request for %s is already open: %s", res.Branch, pr.URL))
		return out, nil
	}
	pr, err := host.Create(ctx, NewPR{
		Head: res.Branch, Base: base, Draft: draft,
		Title: Sanitize(oneLine(title, 250)), Body: Body(res, base, host.MaxBody()),
	})
	if err != nil {
		return nil, err
	}
	out.URL, out.Number, out.Draft, out.Created = pr.URL, pr.Number, pr.Draft, true
	if draft && !pr.Draft && o.Remote.Platform == GitHub {
		log("this repository does not allow draft pull requests; opened a regular one")
	}
	log(fmt.Sprintf("opened #%d: %s", pr.Number, pr.URL))
	return out, nil
}

// Body is the pull request description: what the agent says it did, the
// task, the files, the checks zenith ran, and the commands the agent ran.
func Body(res *protocol.Result, base string, limit int) string {
	var b strings.Builder
	b.WriteString("> **Written by an AI agent ([Zenith](https://github.com/divmora/zenith)).** Review it like any other contribution: the summary below is the agent's own account.\n\n")
	if res.Status != protocol.StatusDone {
		fmt.Fprintf(&b, "> **The run did not finish** (status: %s).", res.Status)
		if res.Error != "" {
			b.WriteString(" " + oneLine(res.Error, 300))
		}
		b.WriteString("\n\n")
	}
	if s := strings.TrimSpace(res.Summary); s != "" {
		b.WriteString("## Summary\n\n" + clip(s, 20000) + "\n\n")
	}
	b.WriteString("## Task\n\n" + fenced(clip(strings.TrimSpace(res.Task), 8000), "text") + "\n")
	if res.Plan != "" {
		b.WriteString("<details><summary>Approved plan</summary>\n\n" + clip(res.Plan, 12000) + "\n\n</details>\n\n")
	}

	if len(res.Files) > 0 {
		fmt.Fprintf(&b, "## Files changed (%d)\n\n", len(res.Files))
		for i, f := range res.Files {
			if i == 50 {
				fmt.Fprintf(&b, "- ... and %d more\n", len(res.Files)-50)
				break
			}
			b.WriteString("- " + code(f) + "\n")
		}
		b.WriteString("\n")
	}

	if final := finalChecks(res.Checks); len(final) > 0 {
		b.WriteString("## Checks run by Zenith\n\n| Command | Before the change | After |\n|---|---|---|\n")
		before := map[string]protocol.Check{}
		for _, c := range res.Checks {
			if c.Stage == protocol.CheckBaseline {
				before[c.Command] = c
			}
		}
		for _, c := range final {
			was := "not run"
			if p, ok := before[c.Command]; ok {
				was = checkState(p)
			}
			fmt.Fprintf(&b, "| %s | %s | %s |\n", tableCode(c.Command), was, checkState(c))
		}
		b.WriteString("\n")
	} else {
		b.WriteString("## Checks\n\nZenith found no checks to run for this repository, so nothing was verified automatically.\n\n")
	}

	if len(res.Commands) > 0 {
		fmt.Fprintf(&b, "<details><summary>Commands the agent ran (%d)</summary>\n\n", len(res.Commands))
		b.WriteString(fenced(clip(strings.Join(res.Commands, "\n"), 15000), "sh"))
		b.WriteString("\n</details>\n\n")
	}

	b.WriteString("---\n")
	meta := []string{"Model: " + code(res.Model)}
	if res.Persona != "" {
		meta = append(meta, "Persona: "+code(res.Persona))
	}
	meta = append(meta, fmt.Sprintf("Steps: %d", res.Steps), fmt.Sprintf("Tokens: %d", res.Usage.Total()))
	if res.BaseSHA != "" {
		meta = append(meta, "Base: "+code(short(res.BaseSHA))+" ("+code(base)+")")
	}
	b.WriteString(strings.Join(meta, " · ") + "\n")

	body := Sanitize(b.String())
	if len(body) > limit {
		body = clip(body, limit-200) + "\n\n_(description cut to fit the size limit)_\n"
	}
	return body
}

// finalChecks returns the last final round.
func finalChecks(checks []protocol.Check) []protocol.Check {
	last := -1
	for _, c := range checks {
		if c.Stage == protocol.CheckFinal && c.Round > last {
			last = c.Round
		}
	}
	var out []protocol.Check
	for _, c := range checks {
		if c.Stage == protocol.CheckFinal && c.Round == last {
			out = append(out, c)
		}
	}
	return out
}

func checkState(c protocol.Check) string {
	switch {
	case c.Passed:
		return "✅ passed"
	case c.TimedOut:
		return "⏱️ timed out"
	case c.PreExisting:
		return fmt.Sprintf("⚠️ failed (exit %d), already failing before", c.ExitCode)
	default:
		return fmt.Sprintf("❌ failed (exit %d)", c.ExitCode)
	}
}

// fenced wraps s in a code fence longer than any backtick run inside it,
// so the content cannot close the fence.
func fenced(s, lang string) string {
	n, run := 3, 0
	for _, c := range s {
		if c == '`' {
			run++
			n = max(n, run+1)
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", n)
	return f + lang + "\n" + s + "\n" + f + "\n"
}

// code renders s as inline code.
func code(s string) string {
	s = oneLine(s, 300)
	n, run := 1, 0
	for _, c := range s {
		if c == '`' {
			run++
			n = max(n, run+1)
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", n)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	return f + s + f
}

// tableCode is code() safe inside a table cell.
func tableCode(s string) string { return strings.ReplaceAll(code(s), "|", `\|`) }

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	return clip(s, n)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "..."
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
