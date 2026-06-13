# AGENTS.md

This file provides guidance to AI agents working in this repository. Pebble is
a Linux service manager with layered configuration and an HTTP API, written in
Go. See [`HACKING.md`](HACKING.md) for the full developer guide and
[`STYLE.md`](STYLE.md) for the Go style guide; this file only collects the
high-signal commands and conventions.

## Build and run

```bash
go build ./cmd/pebble          # build the binary
go run ./cmd/pebble            # run the CLI (recompiles on change)
PEBBLE=~/pebble go run ./cmd/pebble run   # run the daemon ($PEBBLE must be set)
```

## Test

```bash
go test -race ./...                          # unit tests (CI runs with -race)
go test ./internals/cli -check.f PebbleSuite # single gocheck suite or test
go test -count=1 -tags=integration ./tests/  # integration tests (build tag)
```

Some tests need root and read two env vars for the unprivileged user:

```bash
PEBBLE_TEST_USER=$USER PEBBLE_TEST_GROUP=$USER sudo -E -H "$(which go)" test ./...
```

Tests use [`gopkg.in/check.v1`](https://pkg.go.dev/gopkg.in/check.v1), dot-imported
(`. "gopkg.in/check.v1"`), not the stdlib `testing` assertions.

## Lint and format

```bash
go fmt ./...     # CI fails on any resulting diff
go install honnef.co/go/tools/cmd/staticcheck@v0.7.0 && staticcheck ./...
go install golang.org/x/vuln/cmd/govulncheck@v1.1.4 && govulncheck ./...
```

CI also rejects any use of `interface{}` — write `any`.

## Conventions

- **Commits / PR titles:** [Conventional Commits](https://www.conventionalcommits.org/);
  scopes are optional (e.g. `feat(daemon): …`). Prefixes: `fix`, `feat`,
  `build`, `chore`, `ci`, `docs`, `style`, `refactor`, `perf`, `test`.
- **Error messages** are lowercase and start with "cannot" (`cannot create
  log client: %w`); **log messages** are capitalised and start with "Cannot".
- **Imports** are in three alphabetised groups: stdlib, third-party, then
  `github.com/canonical/pebble/...`.

## Docs

Docs are Sphinx, under `docs/`. Run `make run` in `docs/` to build. After
changing a CLI command, run `make cli-help` in `docs/` (CI fails if the
generated CLI reference is stale).
