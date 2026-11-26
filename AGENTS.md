# AGENTS.md — Zenith

Guidance for AI coding agents and contributors. Org-wide rules live in
[divmora/.github AGENTS.md](https://github.com/divmora/.github/blob/main/AGENTS.md).
The design and milestones are in [docs/DESIGN.md](docs/DESIGN.md).

## Layout

```
cmd/zenith/         CLI: run (one task → branch + commit; --approve-plan stops after a plan),
                    resume (--approve [--note] | --feedback), version; human progress on stderr
pkg/llm/            provider-neutral chat client with tool calling + streaming:
                    openai (+ compatible: azure, ollama, openrouter, groq, deepseek, mistral,
                    xai, together, litellm, openai-compatible), anthropic, gemini, bedrock
pkg/llm/llmtest/    scripted fake client for deterministic agent-loop tests
pkg/protocol/       the CLI contract: JSON events, result file, exit codes (the console imports it)
internal/harness/   the agent loop: plan/execute phases, budgets, nudges, checkpoints, branch + commit,
                    verify-and-fix rounds (verify.go), context compaction (compact.go)
internal/verify/    detects the project's check command (make test, go test, npm test, ...) and runs it
internal/tools/     read_file, list_files, grep, write_file, edit_file, run_command, finish
internal/policy/    workspace confinement, command env allowlist, output truncation
internal/gitx/      git for zenith itself (hooks/fsmonitor/filters disabled, scrubbed env) and a guard
                    that snapshots .git/config, info/attributes and hooks and restores them after a run
internal/prompt/    system prompt (+ persona role, AGENTS.md / .zenith/notes.md / .zenith/<persona>.md as marked repo guidance)
internal/persona/   built-in personas (builtin/*.md, embedded) and the persona file format for --persona-file
internal/version/   build version
docs/               design and plan
```

### `pkg/llm` rules

- One `Chat(ctx, req, handler)` per provider: nil handler = non-streaming API, otherwise stream and
  call the handler with deltas. Both paths return the same complete `Response`.
- The assistant `Message` goes back into the history unchanged. `Reasoning` is replayed only to the
  provider that produced it (Anthropic/Bedrock verify signatures; Gemini needs thought signatures).
- Retries live in `WithRetry`, never in providers (Bedrock SDK retries are off). A stream that
  already emitted an event is never retried or failed over.
- Every provider change gets an `httptest` test, in both modes. `TestLive` (opt-in via
  `ZENITH_LIVE_MODELS`) checks real providers.
- Known limits: Bedrock has no "none" tool choice (stays auto); OpenAI-compatible reasoning is
  captured but not sent back.

### `internal/publish`

`zenith publish` pushes `Result.CommitSHA` (the verified commit, not whatever the branch points at now)
to the run's branch over HTTPS via `gitx.Push`, then finds or opens the pull request through the
GitHub or GitLab REST API. The token is read from env only and reaches git as an env-config header scoped
to the push host (never argv); redirects are not followed. Everything posted goes through `Sanitize`
(no @mentions, no GitLab quick actions), because the summary is model-written. Only reads are retried:
a create that timed out may have worked. Tests push to a local bare repository against a fake API.

The image (`Dockerfile`) is built by `docker-publish.yml` into `ghcr.io/divmora/zenith`. Like GLFG,
dispatch that workflow on the `vX.Y.Z` tag after each release.

## Scope

This repo is the **agent and its CLI**: one task per process. Webhooks, queues, dashboards, SSO,
databases and sandbox orchestration belong in Zenith Console (separate repo, Elastic License 2.0).
Don't add a long-running server here.

The CLI contract (flags, `ZENITH_*` env vars, the JSON event stream, the result and checkpoint
files, exit codes) is the interface the console depends on. Once it exists, keep it backward
compatible. Only `pkg/` is public; everything in `internal/` is free to change.

## Invariants (keep them)

- **Repository content is untrusted.** Code, comments, READMEs and issue text can carry prompt injection.
- **No secrets reach the model's tools.** Commands run with a scrubbed environment; API keys and git tokens
  are read only by the CLI itself.
- **zenith verifies, not the model.** After finish, zenith runs the checks itself (baseline first, so
  pre-existing failures don't block), sends new failures back for up to --max-fix-attempts rounds, then
  fails the run. Never let a model's "tests pass" claim stand in for this.
- **Compaction keeps the history valid.** The first message (task) and the last messages stay; roles alternate;
  every tool result follows its call. `alternates()` in the tests checks this; keep it passing.
- **Planning is read-only.** The plan phase offers only read_file, list_files, grep and propose_plan, creates no
  branch and changes nothing, so a checkpoint is just (base commit + conversation) and resumes in a fresh clone.
- **No push during `run`.** Pushing and opening PRs is a separate `publish` step without the model.
- **The user's git is restored.** Every run snapshots the repository's git config, attributes and hooks
  (shared with linked worktrees) and restores them at the end, so a planted fsmonitor or hook never runs in
  the user's own git. `--worktree` runs never touch the user's checkout at all.
- **zenith's own git calls go through `internal/gitx`.** The model can rewrite `.git/config` and plant hooks
  with run_command; gitx disables everything git would execute and never passes secrets.
- **Workspace confinement.** File tools resolve symlinks and stay inside the workspace; `.git/` is not writable by tools.

## Personas

Built-ins live in `internal/persona/builtin/*.md` (front matter + instructions). Keep them about *what* to
look for and *how careful* to be; never tell the model to branch, commit, push or open PRs (zenith does that).
Each says what to do when nothing is worth changing. `TestBuiltin` checks both. Live runs showed that tests
passing is not enough (Sentinel once shipped a fix that broke the default config), so the base prompt and
Sentinel insist on checking real defaults.

## Conventions

- Go 1.27, stdlib first. `log/slog` everywhere. Wrap errors with context; propagate `context.Context`.
- `make test` runs everything with `-race`; tests must not need network or credentials.
- Lint with golangci-lint v2 (`make lint`), the same as org CI.
- The version lives in `internal/version`, updated by Release Please and injected by GoReleaser.
- Conventional Commits for commits and PR titles.
