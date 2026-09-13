# ADR-0008 — Coarse IAM actions are resource-scoped

**Status:** Accepted · **Date:** 2026-09-13 · **Builds on:** [ADR-0007](0007-allow-list-over-name-prefix.md)

## Context

ADR-0007 made the allow-list the single source of the shipped IAM policy, with
every action granted on `Resource: "*"`. That works while each IAM action maps to
one read operation: `ecs:DescribeServices` permits exactly that call.

API Gateway breaks the assumption. Its IAM model is verb-based: every read, of
every kind of API Gateway resource, is `apigateway:GET`. Granted on `"*"`, it
permits `GET /apikeys?includeValues=true` — reading API key values — along with
usage plans, custom domain names and client certificates. The guard never makes
those calls, but the policy is the artifact a security team approves, and it
would grant them.

## Decision

A service whose IAM action is coarser than its operations declares
`IAMResources`, and the policy generator emits a separate statement scoping its
actions to those ARNs. For API Gateway:

```
apigateway:GET on arn:aws:apigateway:*::/restapis, /restapis/*, /apis, /apis/*
```

— the API definitions, and nothing else under `apigateway`.

Two services may share one coarse action (API Gateway v1 and v2 both use
`apigateway:GET`), but only when every service granting it declares it
explicitly; a *derived* action granted twice remains a validation error.
Writing verbs (`apigateway:POST/PUT/PATCH/DELETE`) are on the forbidden list.

`scan --dry-run` prints the scope next to the operations, so a reviewer sees
"GET, only on API definitions" rather than a bare `GET`.

## Consequences

**Good**
- The policy grants what the collectors need and not what they could be
  tricked into needing. Proven against a real account by
  [`spikes/m1/22-probe-scope.sh`](../../spikes/m1/22-probe-scope.sh): with the
  scanner's own credentials, API definitions are readable and API key values,
  usage plans, domain names and client certificates are refused.
- A scan with only the scoped policy matched a full-admin scan field for field.

**Bad**
- Custom domains (`/domainnames`) are outside the scope, so the mapping from a
  custom domain to an API is not collected. Adding it means widening the scope
  deliberately — see [Q15](../09-open-questions.md).
- The policy is now several statements instead of one; slightly more to read.

## Alternatives rejected

- **Keep `"*"` and rely on the guard** — the guard protects cloud-echo, not the
  role. Anyone holding the role could read API key values.
- **A `Deny` on `/apikeys*` next to an `Allow` on `"*"`** — deny-listing fails
  open on the next sensitive path API Gateway adds. An allow-listed scope fails
  closed.
