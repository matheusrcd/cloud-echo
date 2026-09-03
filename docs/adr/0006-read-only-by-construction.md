# ADR-0006 — Read-only by construction

**Status:** Accepted · **Date:** 2026-08-27 · **Refined by:** [ADR-0007](0007-allow-list-over-name-prefix.md)

> Layer 1 below describes a name-prefix check. Implementation showed that check
> would permit `secretsmanager:GetSecretValue`, contradicting Guarantee 2.
> [ADR-0007](0007-allow-list-over-name-prefix.md) replaces it with an explicit
> allow-list and demotes the prefix to defence in depth. The three-layer structure
> and everything else here still stand.

## Context

cloud-echo asks developers to point it at a production AWS account. Adoption
depends on the answer to "what can this thing do to my account?" being *provably*
"nothing."

A README promise is not enough. Security teams reject tools on the basis of what
they *could* do, not what they intend to do.

## Decision

Read-only is enforced structurally, in three layers:

1. **SDK middleware guard.** The discovery AWS client carries a middleware that
   rejects any operation whose name is not `Describe*`, `List*`, `Get*`, or
   `BatchGet*`. **There is no flag to disable it** — no `--force`, no env var.
2. **Separate client types.** The materializer uses a different client type whose
   endpoint resolver refuses anything outside `localhost` and the project docker
   network. The read path and the write path are not the same type, so they cannot
   be confused by a refactor.
3. **A least-privilege policy the project ships.**
   `policies/cloud-echo-scanner.json` lists exactly the actions the collectors
   use, with a CI test that fails if a collector calls something the policy
   doesn't grant, or the policy grants something no collector uses.

Additionally: `secretsmanager:GetSecretValue` is not in the policy and not in the
guard's allow-list, so secret values cannot be read even by accident.

## Consequences

**Good**
- The guarantee is auditable in about a minute by someone who does not trust us.
  That is the whole point.
- Removes the largest objection to adoption inside an organisation.
- Prevents a whole class of catastrophic bug: no refactor can accidentally
  introduce a mutating call to production.
- The shipped policy gives security teams something concrete to approve, instead
  of `ReadOnlyAccess`, which is far broader and correspondingly harder to sign
  off.

**Bad**
- `ecr:GetAuthorizationToken` is needed for image pulls and reads as a
  token-issuing call. It is still read-only and grants only pull rights, but it
  requires explaining in the docs rather than hiding.
- The naming-convention check is a heuristic. A hypothetical AWS operation named
  `GetSomethingThatMutates` would pass. Mitigated by the explicit policy being the
  real boundary — the middleware is defence in depth, not the only control.
- Some future features (importing real data, dumping an RDS schema) sit outside
  this boundary. That is intentional: they must require a *distinct* IAM
  permission the scanner policy does not grant, so an organisation can allow
  scanning while forbidding extraction.

## Alternatives rejected

- **Document it and trust developers** — free, and worthless to a security
  reviewer.
- **Rely on the user's IAM policy alone** — correct in principle, but users will
  run this with admin credentials because that's what they have. The tool must be
  safe even then.
- **A `--allow-writes` escape hatch for future features** — the moment it exists,
  the guarantee is "read-only unless someone passes a flag," which is not a
  guarantee.
