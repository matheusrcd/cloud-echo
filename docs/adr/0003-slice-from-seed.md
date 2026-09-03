# ADR-0003 — Materialize a slice from a seed resource

**Status:** Accepted · **Date:** 2026-08-27

## Context

The obvious framing — "scan the account and rebuild it locally" — does not survive
contact with a real AWS account. A mid-size account has hundreds of resources and
tens of gigabytes of container images. Nobody's laptop runs that, and more
importantly nobody can *review* the result to decide whether it's right.

## Decision

Discovery scans broadly. **Materialization is scoped**: the user picks one or more
seed resources and a traversal depth, and cloud-echo expands through the inferred
graph to produce a slice.

```
cloud-echo scan                                    # whole account → inventory
cloud-echo plan --seed ecs/orders-api --depth 2    # slice → blueprint
cloud-echo up                                      # run the slice
```

Expansion rules:

- Depth counts hops through graph edges from the seed.
- Stateful dependencies (tables, queues, databases, caches) at depth ≤ N are
  materialized in full — they're cheap.
- Workloads (ECS/Lambda) at the depth boundary are **not** started; they become
  auto-generated mocks in the Echo Gateway.
- Async consumers get a depth bonus: a queue's consumer is included at depth
  N, because a queue nobody drains is a broken environment, not a smaller one.
- Only `certain` and `high` confidence edges expand automatically. `medium`
  is included but flagged; `low` is offered as a suggestion.

## Consequences

**Good**
- Runs on a laptop. Boots in minutes rather than never.
- The blueprint is small enough to read in a PR, which is what makes it
  trustworthy.
- The boundary becomes a feature: everything outside the slice is an explicit,
  inspectable, editable mock rather than a missing piece. This is the same
  mechanism as third-party mocking, so it costs nothing extra to build.
- Matches how developers actually work — on one service at a time.
- Scales naturally: `--depth 3`, or multiple seeds, or eventually the whole
  account if someone really wants it.

**Bad**
- A slice can miss a dependency the linker didn't infer. Mitigated by the gateway
  logging unmatched egress: an unexpected call to an unmocked host is a visible
  event, and the fix is `plan --seed ... --add <node>`.
- Choosing the right depth is a judgement call. Mitigated by `plan --dry-run`
  showing what each depth would include, with resource-footprint estimates.
- Multi-entrypoint architectures need several seeds. Supported, but the user has
  to know that.

## Alternatives rejected

- **Whole account with filters** — better demo, unusable in practice. Filters by
  tag or VPC are a blunt instrument that either includes too much or cuts a
  dependency chain arbitrarily.
- **Inventory and graph only, no materialization** — ships faster and is a real
  product on its own (and is effectively what M1 delivers), but it isn't the
  project.
- **Manual selection with no traversal** — puts the burden of knowing the topology
  back on the user, which is the exact problem cloud-echo exists to solve.
