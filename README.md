# Zenith

[![CI](https://github.com/divmora/zenith/actions/workflows/ci.yml/badge.svg)](https://github.com/divmora/zenith/actions/workflows/ci.yml)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

**Zenith is an autonomous coding agent.** Give it a task and a repository: it plans the change, waits for
your approval if you ask it to, edits the code, runs the project's tests and linters, and hands back a branch
ready for a pull request.

- **One task per process**, built to run inside a sandbox (Docker or Kubernetes).
- **Any model provider**: OpenAI and compatible servers, Anthropic, Google Gemini, AWS Bedrock.
- **Plan first**: an optional read-only planning phase you approve before any file changes.
- **Safe by design**: the model's tools never see your API keys or git credentials; pushing is a separate step.

> **Status: early development.** `run`, `resume` and `publish` work; the CLI contract may still change
> before 1.0. The roadmap and design are in [docs/DESIGN.md](docs/DESIGN.md).

## Usage

```bash
export ZENITH_MODEL=bedrock:qwen.qwen3-coder-next   # or anthropic:<model>, openai:<model>, gemini:<model>, ...

# Do a task: zenith creates a branch, works, tests, and commits there.
zenith run --workspace ./repo --task "Fix the off-by-one in pagination and add a test"

# Same, but in a new git worktree: your checkout (branch, uncommitted changes) is never touched.
zenith run --worktree --workspace ./repo --task "..."

# Or let a built-in specialist pick and do one focused improvement (no task text needed):
zenith personas                                  # sentinel, bolt, palette, sweeper, probe, scribe
zenith run --persona sentinel --worktree --workspace ./repo

# Or review a plan first: planning is read-only and exits 10 with a checkpoint.
zenith run --workspace ./repo --approve-plan --checkpoint /tmp/plan.json --task "..."
zenith resume --checkpoint /tmp/plan.json --feedback "Keep the public API unchanged"   # revised plan, exit 10 again
zenith resume --checkpoint /tmp/plan.json --approve --note "Name the helper clampPage"  # implement + commit, exit 0
```

After the agent says it is done, zenith runs the project's own checks (`make test`, `go test`, `npm test`, ...;
override with `--verify CMD`) and sends failures back to the agent to fix (`--max-fix-attempts`, default 3).
Checks that already failed before the change don't count against it. Long runs are compacted to fit
`--context-tokens`.

Progress goes to stderr; the result JSON to stdout (or `--result FILE`); `--events FILE` writes one JSON event
per line. Exit codes: 0 done, 10 awaiting approval, 20 budget exceeded, 30 failed, 1 internal error, 2 usage.

### Open the pull request

`run` never gets push credentials. Publishing is a separate step without a model, so run it outside the
agent's sandbox:

```bash
zenith run --worktree --workspace ./repo --result /tmp/result.json --task "..."
GITHUB_TOKEN=... zenith publish --result /tmp/result.json            # GitLab: GITLAB_TOKEN
```

It pushes the commit zenith verified to the run's branch over HTTPS and opens a pull request (a merge request
on GitLab) against the run's base branch, or the default branch if the remote doesn't have it. The
description says an agent wrote it and lists the agent's summary, files, the checks zenith ran (before and
after), and the commands the agent ran; @mentions and GitLab quick actions in it are neutralised. Running it
again reports the pull request that is already open. Other flags: `--remote URL` (default: `origin`),
`--base`, `--title`, `--draft`, `--allow-incomplete` (a failed run, as a draft), `--platform` and `--api-url`
for GitHub Enterprise and self-managed GitLab.

## Install

```bash
go install github.com/divmora/zenith/cmd/zenith@latest
zenith version
```

Or use the container image, which also has Go, Node.js (npm, pnpm, Yarn 1), Python with pytest, make and
gcc, for building and testing the repository:

```bash
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD:/workspace" \
  -e ZENITH_MODEL -e ANTHROPIC_API_KEY ghcr.io/divmora/zenith run --task "..."
```

For other stacks, build `FROM ghcr.io/divmora/zenith` and add yours. Limit the container's network to the
model provider and package registries.

Or build from source:

```bash
make build
./bin/zenith version
```

## Zenith Console

Zenith Console (separate repository) runs Zenith for teams: task intake from issues and chat, a queue,
sandboxed runs, plan approval in the browser, and history. This repository is the agent and its CLI only.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and [AGENTS.md](AGENTS.md). Report security issues privately as
described in [SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE). Copyright 2026 DIVMORA Technologies.
