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

**M1 in progress.** Discovery covers **ECS, SQS, DynamoDB, Lambda, IAM and API
Gateway (v1 and v2)** — with secret-shaped values redacted before
anything reaches disk. It has been validated against a real account — with only
the shipped policy, and through a round trip into Floci:
[m1-real-account-findings.md](spikes/m1-real-account-findings.md).

**The linker reads three tiers**: Tier 1 (relationships the account declares),
Tier 2 (what configuration names) and Tier 3 (what roles permit — the only tier
that knows *what* a workload does with what it names), plus flow classification,
each validated against a real account. What M1 still lacks is breadth — the RDS,
ElastiCache and supporting collectors — and the exit criterion: someone who knows
a real account confirming its graph. See [08-roadmap.md](08-roadmap.md).

## Conventions

- **ADRs are append-only.** To reverse a decision, write a new ADR that
  supersedes the old one. Never edit history.
- **Every inferred fact carries evidence.** If cloud-echo claims service A talks
  to table B, it must be able to show the exact API response field that says so.
- **The blueprint is the contract.** Every other component either produces it or
  consumes it. Nothing bypasses it.
