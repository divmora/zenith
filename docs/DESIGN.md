# Zenith Agent: Design and Plan

Status: M0–M5 built (plus worktree mode and personas), 2026-11-26. Publish is tested against fake APIs; a live PR on GitHub and GitLab is still to do, then M6 (dogfood). Covers the **agent repo only** (`divmora/zenith`, Apache 2.0).
`zenith-console` (ELv2: UI, queue, orchestrator) starts after the agent proves itself.

## 1. What it is

An autonomous coding agent. You give it a task and a repository, it plans the change, waits for approval
(optional), edits the code, runs the project's checks, and hands back a branch ready for a PR.

- It runs as **one process per task**, inside a sandbox (Docker or Kubernetes, later started by the console).
- It is fully usable on its own as a CLI, the same way `code-review-bot` is.
- It uses **its own harness**: no LocalHarness dependency.

## 2. Repository layout

Mirrors `code-review-bot` (Go 1.27, release-please, goreleaser, org CI, golangci-lint v2).

```
cmd/zenith/            CLI entry point
pkg/llm/               Providers with tool calling + streaming (public; the console imports it)
pkg/protocol/          Task spec, event schema, checkpoint, exit codes (public; the console contract)
internal/harness/      Agent loop, phases, budgets, context compaction, checkpointing
internal/tools/        view_file, write_file, edit_file, glob, grep, run_command, git_*, propose_plan, finish
internal/policy/       Path confinement, command rules, env scrubbing, output caps
internal/prompt/       System prompts per phase, task templates (Sentinel/Bolt/Palette/Sweeper presets)
internal/publish/      Push branch + open PR (GitHub, GitLab)
internal/version/
examples/  docs/
```

## 3. Provider layer (`pkg/llm`)

Start from `code-review-bot/internal/llm` (config, `provider:model` specs, named profiles, retry,
fallback, Bedrock auth) and add what an agent needs:

- **Tool calling**: tool definitions, assistant tool calls, tool results in the history.
  - OpenAI-compatible: `tools` / `tool_calls` / `role: tool`
  - Anthropic: `tool_use` / `tool_result` blocks
  - Bedrock: Converse `toolConfig`, `toolUse` / `toolResult`
  - Gemini: `functionCall` / `functionResponse`, thought signatures echoed back
- **Streaming** (`Stream`) with text, thinking, and tool-call deltas; plain `Complete` stays for JSON-mode callers.
- **Prompt caching** where supported (Anthropic `cache_control`, Bedrock cache points), with cached tokens reported in usage.
- A **scripted fake provider** for deterministic harness tests.

`code-review-bot` can switch to this package later. That is not part of this plan.

## 4. The CLI contract (what the console will depend on)

```
zenith run      --task "..." --workspace DIR --model SPEC [--approve-plan] [limits] [--events FILE|-]
zenith resume   --checkpoint FILE (--approve | --feedback "...") [--workspace DIR]
zenith publish  --result FILE [--remote URL] [--base B] [--draft]   # push branch + open PR
zenith version
```

- **Inputs**: flags, plus `ZENITH_*` env vars; secrets (API keys, git tokens) come **only** from env vars.
- **Limits**: `--max-steps`, `--max-tokens`, `--max-cost`, `--timeout`.
- **Output**: newline-delimited JSON events (versioned schema in `pkg/protocol`) on stdout or a file;
  human-readable progress on stderr.
  Events: `task_started`, `phase_changed`, `assistant_text`, `tool_call`, `tool_result`, `plan_proposed`,
  `check_result`, `diff`, `usage`, `task_finished`.
- **Result file**: final status, branch name, commit SHA, PR title/body draft, summary, usage, cost.
- **Exit codes**: `0` done · `10` awaiting approval · `20` budget exceeded · `30` task failed · `1` internal error.

### Plan approval across sandboxes

1. `run --approve-plan` → the **plan phase** uses read-only tools only → `propose_plan` → write a checkpoint → exit `10`.
2. The human approves or gives feedback (CLI now, console later).
3. `resume` in a **fresh sandbox** at the same base commit. Nothing was edited during planning, so the
   checkpoint only needs the conversation, the plan, and the base SHA. Sandboxes never sit idle waiting for a human.

## 5. Harness

Phases: `plan` (read-only) → [`awaiting_approval`] → `execute` → `verify` → `finish`.

- **Loop**: model → tool calls (run in parallel only when all of them are read-only) → results → repeat until `finish` or a limit.
- **Verify**: detect the project's checks (Makefile targets, `go test`, `npm test`, linters, or a `.zenith.yml`
  override) and run them; on failure, feed the output back to the model, with a capped number of fix attempts.
- **Context compaction**: when near the window, summarise old turns and truncate large tool outputs
  (full output stays in the event log).
- **Budgets** are checked before every model call; on hitting one, the harness checkpoints and exits `20`.
- **Commits**: one branch `zenith/<slug>`; commits made by the harness, not the model.
- **Repo memory**: optional `.zenith/notes.md` read at the start (like Jules' `.jules/*.md`).

## 6. Security model

The repository content is **untrusted** (prompt injection via code, issues, READMEs).

- `run_command` gets a **scrubbed environment**: no API keys and no git tokens ever reach the model's tools.
- **No push credentials during `run`.** `publish` is a separate, non-LLM step; the console runs it in a trusted context.
- Paths are confined to the workspace (symlinks resolved); writes to `.git/` are blocked.
- Command and output caps (time, size); network isolation is the sandbox's job (documented for Docker/K8s).
- The PR body states it was produced by an agent and lists the commands that were run.

## 7. Milestones

| # | Milestone | Done when |
|---|---|---|
| M0 | Scaffold | Repo files (LICENSE, README, AGENTS.md, SECURITY, CONTRIBUTING), CI, release-please, goreleaser; `zenith version` works |
| M1 | `pkg/llm` with tools + streaming | Multi-turn tool-call tests pass against fakes for all 4 provider families; real runs on Bedrock (Pixelvide test account) and at least one direct API |
| M2 | Harness + tools + `run` | `zenith run` completes a small real task end-to-end and commits on a branch; events + result file written |
| M3 | Plan / approve / resume | Plan phase is read-only; `resume --approve` and `--feedback` work from a fresh clone |
| M4 | Verify + compaction + budgets | Failing tests are fixed in-loop; long tasks stay within the context window; every limit has a test |
| M5 | Publish + image | `zenith publish` opens PRs on GitHub and GitLab; `ghcr.io/divmora/zenith` image with common toolchains |
| M6 | Dogfood | Go/no-go for the console (below) |

## 8. Go / no-go for the console

Run Zenith on ~10 real tasks across Divmora repos (bug fixes, small features, the four Jules presets). Proceed to
`zenith-console` when most PRs are mergeable with light edits and cost per task is acceptable. Record results in `docs/dogfood.md`.

## 9. Open questions

- Default model(s) and price limits per task.
- Whether `publish` should also support Bitbucket in v1 (code-review-bot already has a Bitbucket client).
- First commit to an empty repo: `main` has no commits, so the scaffold can't arrive by PR. Push one initial
  commit (LICENSE + README) directly, then everything else by PR?
