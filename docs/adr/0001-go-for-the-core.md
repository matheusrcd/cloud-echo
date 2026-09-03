# ADR-0001 — Go for the core

**Status:** Accepted · **Date:** 2026-08-27

## Context

cloud-echo is a long-running local daemon that fans out hundreds of concurrent
AWS API calls, drives the Docker Engine API, runs an HTTP proxy in the hot path of
application traffic, and is distributed to developers who should not have to
install a runtime first.

## Decision

Go 1.23+ for the CLI, scanner, linker, materializer, and gateway. React + Vite for
the web UI only, served by the Go binary.

## Consequences

**Good**
- Single static binary. `brew install` / download-and-run, no Node, no Python, no
  virtualenv. For a tool developers try once and abandon on friction, this matters
  more than it looks.
- `aws-sdk-go-v2` is mature, has per-operation middleware (which
  [ADR-0006](0006-read-only-by-construction.md) depends on), and has first-class
  pagination and adaptive retry.
- Goroutines make the scanner's fan-out trivial and bounded.
- `github.com/docker/docker/client` is the reference Docker client.
- A performant HTTP proxy is idiomatic Go rather than a fight.
- Cross-compilation to darwin/linux × amd64/arm64 is one build matrix.

**Bad**
- Two languages in the repo. UI contributors and core contributors have different
  toolchains. Mitigated by keeping the UI a thin client over the control API.
- Go's YAML handling requires care to get deterministic output — and
  [04-blueprint.md](../04-blueprint.md) makes determinism a hard requirement.
- More verbose than TypeScript for the linker's rule logic.

## Alternatives rejected

- **TypeScript/Node** — one language end to end and easier contribution, but
  requires Node on every user's machine, has weaker process/container lifecycle
  control, and would make the gateway's performance target harder.
- **Python** — `boto3` is the most complete AWS SDK and ideal for prototyping the
  linker, but distribution is the worst of the options and it is a poor fit for a
  long-lived daemon.
- **Rust** — excellent binary and performance story, but the AWS SDK is less
  mature and iteration speed matters more than raw performance in a project that
  is overwhelmingly I/O and heuristics.
