package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/divmora/zenith/internal/gitx"
	"github.com/divmora/zenith/internal/publish"
	"github.com/divmora/zenith/pkg/protocol"
)

const publishUsage = `Usage: zenith publish --result FILE [flags]

Push the branch a run produced and open a pull request (a merge request on
GitLab) for it. No model is involved: this is the step that holds the
code-host token, so run it outside the agent's sandbox.

It pushes the commit recorded in the result (the one zenith verified) over
HTTPS, then opens a pull request against the run's base branch if the
remote has it, else the repository's default branch. If a pull request for
the branch is already open, it is reported and nothing new is created.

The token comes only from the environment:
  GitHub: ZENITH_GITHUB_TOKEN, GITHUB_TOKEN or GH_TOKEN (needs contents and pull-requests write)
  GitLab: ZENITH_GITLAB_TOKEN or GITLAB_TOKEN (api and write_repository scopes)

Output: progress on stderr; the outcome JSON (url, number, ...) on stdout or
in --output.

Flags:
`

func cmdPublish(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr, publishUsage)
		fs.PrintDefaults()
	}
	var (
		resultPath, workspace, remote, platform, apiURL, base, title, output string
		draft, allowIncomplete, quiet                                        bool
		timeout                                                              time.Duration
	)
	fs.StringVar(&resultPath, "result", "", "the result JSON written by zenith run or resume (required)")
	fs.StringVar(&workspace, "workspace", "", `repository that has the run's commit (default: the result's "workspace")`)
	fs.StringVar(&remote, "remote", "", "repository URL to publish to (default: the origin remote of the workspace)")
	fs.StringVar(&platform, "platform", "", "github or gitlab (default: from the remote's host)")
	fs.StringVar(&apiURL, "api-url", "", "API base URL for GitHub Enterprise or self-managed GitLab, if not the usual one")
	fs.StringVar(&base, "base", "", "branch to merge into (default: the run's base branch, else the default branch)")
	fs.StringVar(&title, "title", "", "pull request title (default: the commit subject)")
	fs.BoolVar(&draft, "draft", false, "open the pull request as a draft")
	fs.BoolVar(&allowIncomplete, "allow-incomplete", false, "publish a run that failed or ran out of budget (always as a draft)")
	fs.StringVar(&output, "output", "", "write the outcome JSON to this file instead of stdout")
	fs.DurationVar(&timeout, "timeout", 5*time.Minute, "give up after this long")
	fs.BoolVar(&quiet, "quiet", false, "no progress on stderr")
	if code, ok := parse(fs, args, stderr); !ok {
		return code
	}
	if resultPath == "" {
		return usageErr(stderr, "--result is required")
	}
	data, err := os.ReadFile(resultPath)
	if err != nil {
		return usageErr(stderr, "read result: %v", err)
	}
	var res protocol.Result
	if err := json.Unmarshal(data, &res); err != nil {
		return usageErr(stderr, "read result %s: %v", resultPath, err)
	}
	if res.V != protocol.Version {
		return usageErr(stderr, "result %s has version %d; this zenith reads version %d", resultPath, res.V, protocol.Version)
	}
	if err := publish.Check(&res, allowIncomplete); err != nil {
		return usageErr(stderr, "%v", err)
	}

	dir := firstSet(workspace, res.Workspace, res.Worktree, ".")
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	if remote == "" {
		url, err := gitx.Git{Dir: dir}.Run(ctx, "remote", "get-url", "origin")
		if err != nil {
			return usageErr(stderr, "no --remote given and %s has no origin remote", dir)
		}
		remote = url
	}
	r, err := publish.ParseRemote(remote, platform, apiURL)
	if err != nil {
		return usageErr(stderr, "%v", err)
	}
	token := ""
	for _, name := range publish.TokenEnv(r.Platform) {
		if token = os.Getenv(name); token != "" {
			break
		}
	}

	if token == "" {
		return usageErr(stderr, "no %s token: set %s", r.Platform, strings.Join(publish.TokenEnv(r.Platform), " or "))
	}

	log := func(s string) {
		if !quiet {
			_, _ = fmt.Fprintln(stderr, "zenith: "+s)
		}
	}
	out, err := publish.Publish(ctx, publish.Options{
		Result: &res, Dir: dir, Remote: r, Token: token, Base: base, Title: title,
		Draft: draft, AllowIncomplete: allowIncomplete, Log: log,
	})
	if err != nil {
		if errors.Is(err, publish.ErrNotPublishable) {
			return usageErr(stderr, "%v", err)
		}
		_, _ = fmt.Fprintf(stderr, "zenith: publish failed: %v\n", err)
		return protocol.ExitFailed
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	b = append(b, '\n')
	if output == "" {
		_, _ = stdout.Write(b)
		return protocol.ExitDone
	}
	if err := os.WriteFile(output, b, 0o644); err != nil {
		return internalErr(stderr, "write outcome: %v", err)
	}
	return protocol.ExitDone
}

func firstSet(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
