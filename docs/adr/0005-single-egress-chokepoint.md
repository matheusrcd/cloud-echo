# ADR-0005 — Single egress choke point (Echo Gateway)

**Status:** Accepted · **Date:** 2026-08-27

## Context

Workload containers emit several kinds of outbound traffic: AWS API calls,
service-to-service HTTP, calls to dependencies outside the materialized slice, and
third-party API calls. The product promises that *all* of these are visible and
editable.

Handling each class separately would mean a different interception mechanism, a
different config surface, and a different UI per class.

## Decision

Route **all** workload egress through one component, the Echo Gateway, which
classifies each request and routes it to Floci, a sibling container, a mock, or a
real upstream.

Injection into every workload container:
- `AWS_ENDPOINT_URL` → gateway (which forwards to Floci)
- `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` → gateway
- a per-project CA cert plus the runtime-specific env var that trusts it

## Consequences

**Good**
- One interception model, one rule format, one traffic timeline, one UI panel.
  "Edit this integration's response" means the same thing whether the integration
  is Stripe, another team's service, or SQS.
- The slice boundary and third-party mocking are the *same mechanism*
  ([ADR-0003](0003-slice-from-seed.md)), so scoping costs nothing extra to build.
- Unmatched egress becomes an observable event: the environment can tell you what
  dependency you forgot instead of hanging on a connect timeout.
- Enables things that are otherwise very hard — mutating an SQS message body,
  injecting a throttling error on a DynamoDB write, adding 2 s of latency to one
  downstream — without touching application code.

**Bad**
- The gateway is in the hot path of every AWS call. Mitigated by streaming
  pass-through, pre-compiled rule matching, a bounded ring buffer for the
  timeline, and a < 2 ms p99 pass-through target. If that can't be met, the tap
  becomes sampled rather than the proxy becoming slow.
- The gateway is a single point of failure for the whole environment. Mitigated by
  a "fail open to Floci" mode: if the mock engine errors, AWS traffic still
  forwards.
- TLS interception requires per-runtime CA trust injection, which is fiddly and
  language-specific. Accepted — cloud-echo owns that complexity so users don't.
- Certificate pinning defeats interception entirely. Handled by detecting the
  handshake failure and telling the user to use `tls: tunnel` for that
  integration, rather than emitting an opaque connection reset.

## Alternatives rejected

- **Per-class interception** — AWS via endpoint env, HTTP via a separate mock
  server, in-slice via DNS. Simpler pieces, but three configs, three UIs, and
  no unified traffic view.
- **Sidecar proxy per workload** — better isolation and per-service policy, but
  N containers, N configs, and no single place to watch the whole system.
- **Library-level instrumentation** — highest fidelity, but requires modifying
  application code, which contradicts the premise of running the real image
  unmodified.
