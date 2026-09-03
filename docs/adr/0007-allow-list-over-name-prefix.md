# ADR-0007 — An explicit allow-list, not a name prefix

**Status:** Accepted · **Date:** 2026-08-27 · **Refines:** [ADR-0006](0006-read-only-by-construction.md)

## Context

[ADR-0006](0006-read-only-by-construction.md) established read-only enforcement in
three layers and described layer 1 — the SDK middleware — as rejecting "any
operation whose name is not `Describe*`, `List*`, `Get*`, or `BatchGet*`".

Implementing it exposed a contradiction. That check permits:

| Operation | What it actually does |
| --- | --- |
| `secretsmanager:GetSecretValue` | returns plaintext secret values |
| `sts:GetFederationToken` | mints credentials |
| `sts:GetSessionToken` | mints credentials |
| `ec2:GetPasswordData` | returns an encrypted administrator password |

The first is the exact call [07-security.md](../07-security.md) Guarantee 2 exists
to forbid. So layer 1 as specified would have permitted the thing the security
model most explicitly prohibits, while appearing to be the control that prevented
it. ADR-0006 half-anticipated this — its Consequences call the naming check a
heuristic, and it already refers to "the guard's allow-list" — but the Decision
section described only the prefix.

The prefix is also unsound in the other direction: it is a claim about AWS's
naming discipline across every service, forever. `sqs:ReceiveMessage` is mutating
in effect and does not match; nothing guarantees the converse never happens.

## Decision

The middleware rejects any operation not on an explicit allow-list of exact
`service:Operation` pairs, held in
[`internal/awsx/operations.go`](../../internal/awsx/operations.go).

The prefix check is retained, but demoted: it runs first as defence in depth, and
the allow-list validator refuses at process start to accept an entry that fails
it. Its job is now to catch a bad edit to the allow-list, not to decide anything.

The allow-list is the single source for three artifacts that must agree:

1. what the middleware permits,
2. `policies/cloud-echo-scanner.json`, generated from it,
3. `cloud-echo scan --dry-run` output.

A per-collector test asserts the operations a collector *actually invokes* — as
observed by replaying recorded fixtures — equal the allow-list entries for that
service, in both directions.

## Consequences

**Good**

- The guarantee stops depending on AWS naming conventions and starts depending on
  a list a reviewer can read in full.
- Guarantee 2 becomes enforceable rather than aspirational: `GetSecretValue` is
  denied by the same mechanism as `DeleteTable`.
- The "granted but never called" direction of the drift test removes permissions
  nobody uses. Every unnecessary permission is a reason for a security team to say
  no.
- `--dry-run` cannot understate the tool's surface, because it is rendered from
  the enforcement list itself.

**Bad**

- Adding an API call now means editing two files and regenerating the policy. That
  friction is intentional — each entry is a permission a user must grant — but it
  will be a recurring papercut as collectors are added.
- The fixture-derived drift test requires fixtures rich enough to trigger every
  permitted operation. That is a real constraint on fixture authoring, though it
  also means a permission we cannot demonstrate a use for does not ship.

**Neutral**

- Placement moved to `Initialize/Before`, ahead of the SDK's input validation, so
  a blocked call reports the refusal rather than a field-validation error. This
  is about diagnosis, not safety; anything in `Initialize` precedes network I/O.

## Alternatives rejected

- **Keep the prefix check alone** — permits `GetSecretValue`. Contradicts
  Guarantee 2.
- **Prefix check plus a deny-list** of known-bad `Get*` operations — requires
  enumerating every dangerous read AWS has ever shipped, and fails open on the one
  nobody thought of. An allow-list of ~40 operations fails closed on everything
  else.
- **Have collectors declare their own operations** instead of a central list — a
  declaration can lie about what the code does. Observing actual calls through
  recorded fixtures cannot.
