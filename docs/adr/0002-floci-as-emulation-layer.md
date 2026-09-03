# ADR-0002 — Floci as the emulation layer

**Status:** Accepted · **Date:** 2026-08-27

## Context

cloud-echo needs AWS service emulation. Writing that from scratch is a multi-year
project on its own and is not where the project's value lies.

## Decision

Depend on [Floci](https://floci.io/) for all AWS API emulation. cloud-echo owns
discovery, inference, planning, orchestration, and egress control — and never
re-implements a single AWS API.

```
cloud-echo  →  what exists, what talks to what, how to stand it up, egress control
Floci       →  AWS API surface, service semantics, container lifecycle
```

## Consequences

**Good**
- MIT-licensed, no auth tokens, no feature gates, no telemetry. cloud-echo can be
  genuinely free, which LocalStack Community (sunsetting March 2026) no longer
  allows.
- ~24 ms boot and ~13 MiB idle means the emulator is not the bottleneck.
- Covers the entire v1 target stack: ECS, SQS, Lambda, DynamoDB, RDS, API Gateway,
  ElastiCache.
- Runs *real* backends for the things that matter — Postgres/MySQL for RDS,
  Valkey for ElastiCache, real runtime images for Lambda. Fidelity we'd never
  reach with mocks.
- LocalStack-compatible wire protocol, so users can swap the emulator later
  without cloud-echo changing much.
- The 12-digit-access-key → account-ID behaviour lets local ARNs match production
  ARNs exactly. Significant, and free.

**Bad**
- Hard dependency on a young project. A regression or an abandonment upstream is
  an existential risk.
- Per-service emulation gaps become cloud-echo's problem in the user's eyes.
- No control over the release cadence.

**Mitigations**
- Keep the emulator behind an internal interface so a second backend is possible.
  Do *not* build the abstraction speculatively — just don't scatter Floci
  specifics across the codebase.
- Pin the Floci image version in the blueprint so environments are reproducible.
- Engage upstream early (see [09-open-questions.md](../09-open-questions.md) Q9);
  contribute fixes rather than working around them.
- Be explicit in user-facing errors about which layer failed. "Floci does not
  support this operation" must never look like a cloud-echo bug.

## Alternatives rejected

- **LocalStack** — best coverage, but the Community edition is sunsetting and the
  Pro tier's auth tokens and feature gates are incompatible with a free OSS tool.
- **Build our own emulation** — absurd scope for the value added.
- **Per-service real substitutes only** (Postgres, Redis, ElasticMQ, DynamoDB
  Local) — viable for the data plane but leaves ECS, Lambda, API Gateway, and IAM
  entirely unsolved, which is most of the interesting behaviour.
