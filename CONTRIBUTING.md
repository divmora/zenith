# Contributing

Thanks for helping improve Zenith. Bug reports, fixes and docs improvements are all welcome.

## Prerequisites

- Go 1.27+
- git
- golangci-lint v2 (for `make lint`)

## Workflow

```bash
git clone https://github.com/divmora/zenith.git
cd zenith
make build        # bin/zenith
make test         # go test -race ./...  (no network or credentials needed)
make lint
```

## Pull requests

1. Branch from `main`.
2. Keep changes focused; add or update tests.
3. Update `README.md`, `AGENTS.md` and `docs/` when behaviour, flags or the CLI contract change.
4. Use a [Conventional Commits](https://www.conventionalcommits.org/) PR title (`feat:`, `fix:`, `docs:` ...). Release Please builds the changelog from it.

Read [AGENTS.md](AGENTS.md) for the architecture and the invariants every change must keep.
