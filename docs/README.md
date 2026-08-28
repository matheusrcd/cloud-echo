# cloud-echo — Design Docs

Living design documentation. These files are the source of truth for *why* the
project is shaped the way it is. Code disagreeing with these docs is a bug in one
of the two — fix the mismatch, don't ignore it.

| Doc | What it covers |
| --- | --- |
| [00-vision.md](00-vision.md) | Problem, product shape, explicit non-goals |
| [01-architecture.md](01-architecture.md) | Pipeline, components, data flow |
| [02-discovery.md](02-discovery.md) | The AWS scanner: APIs, permissions, caching |
| [03-linker.md](03-linker.md) | How the dependency graph is inferred |
| [04-blueprint.md](04-blueprint.md) | The blueprint file format (central artifact) |
| [05-materializer.md](05-materializer.md) | Blueprint → running Floci + containers |
| [06-echo-gateway.md](06-echo-gateway.md) | Egress interception, mocks, record/replay |
| [07-security.md](07-security.md) | Trust model, data handling, hard guarantees |
| [08-roadmap.md](08-roadmap.md) | Milestones and what "done" means per stage |
| [09-open-questions.md](09-open-questions.md) | Unresolved decisions, parked ideas |
| [adr/](adr/) | Architecture Decision Records — dated, immutable |
| [spikes/](spikes/) | Time-boxed investigations and their write-ups |

## Current state

**M0 spike complete** (2026-08-27) — the Floci assumptions the architecture rests
on are validated. Results and the four design changes that came out of it:
[m0-findings.md](spikes/m0-findings.md). The probe suite in
[`spikes/m0/`](../spikes/m0/) runs in ~60 s and doubles as a regression test
against future Floci releases.

**M1 in progress.** The discovery foundation is in: read-only guard, inventory
model, scan runner, generated IAM policy, and the ECS collector with fixture-based
contract tests. Next up in M1 is the rest of the collectors, then the linker —
the part that decides whether the whole premise holds. See
[08-roadmap.md](08-roadmap.md).

## Conventions

- **ADRs are append-only.** To reverse a decision, write a new ADR that
  supersedes the old one. Never edit history.
- **Every inferred fact carries evidence.** If cloud-echo claims service A talks
  to table B, it must be able to show the exact API response field that says so.
- **The blueprint is the contract.** Every other component either produces it or
  consumes it. Nothing bypasses it.
