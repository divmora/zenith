# Security Policy

## Supported versions

Security fixes go into the latest minor release ([see Releases](https://github.com/divmora/zenith/releases/latest)). Upgrade to it to get them; older releases are not patched.

## Reporting a vulnerability

Please **do not open a public issue**. Report privately:

- Email `security@divmora.com`, or
- open a private advisory at [github.com/divmora/zenith/security/advisories/new](https://github.com/divmora/zenith/security/advisories/new).

Include a description, impact, reproduction steps and any suggested fix. We acknowledge within 48 hours, triage within 5 business days, and coordinate disclosure with you.

## Operating Zenith safely

- **Run it in a sandbox.** Zenith edits files and runs commands chosen by a model. Use a disposable container or VM with only the repository mounted, and restrict network access to your model provider and package registries.
- **Repository content is untrusted.** Treat everything the agent produces as a proposal: review the branch before merging.
- **What leaves your network**: files the agent reads and command output are sent to the configured model provider. Pick a provider and data-retention terms that fit your code's sensitivity, or run a local model.
- **Credentials**: pass API keys and git tokens as environment variables only. Zenith keeps them out of the environment of commands the model runs (an allowlist; add more with `--pass-env`), and its own git calls disable hooks, fsmonitor and filters the model could plant. After every run zenith restores the repository's `.git/config`, `info/attributes` and hooks to what they were at the start (and says so in a warning event), so nothing planted runs later in your own git.
- **Publishing**: `zenith publish` is the only command that uses a code-host token, and no model runs in it. Run it outside the agent's sandbox, with a token scoped to the one repository (contents and pull requests). It sends the token to git as an HTTP header scoped to the push host, through the environment rather than the command line, and does not follow redirects.
- **Known limit: same-user processes.** Commands run as the same OS user as zenith, so a determined command can still read zenith's own environment (`/proc/<pid>/environ` on Linux) and files in `$HOME` such as `~/.aws`. Don't give the sandbox long-lived credentials: use short-lived ones, or route model calls through a proxy outside the sandbox (the console will do this).
