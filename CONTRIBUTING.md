# Contributing to cloud-echo

Thanks for looking. cloud-echo is early — the design is settled and validated, but
most of it is not built yet. That shapes what is useful to contribute.

## What helps most right now

**Design feedback.** The riskiest part of this project is the linker: inferring
what talks to what from a read-only scan. If you have an AWS account whose
topology you know well and an opinion about whether the approach in
[docs/03-linker.md](docs/03-linker.md) would work on it, that is worth more than
code. Open an issue.

**Collectors.** [docs/08-roadmap.md](docs/08-roadmap.md) M1 lists the services
still needed. ECS is done and is the reference implementation — see the walkthrough
below.

**Bug reports against the M0 spike.** `spikes/m0/` runs in about a minute against
a real Floci container. If it fails on your machine, that is a finding.

Please open an issue before starting anything large. The design docs are opinionated
and it would be a shame for you to build something that conflicts with them.

## Setup

Go 1.24 or newer. No other tooling required.

```bash
go build ./cmd/cloud-echo
go test ./...
```

Before opening a PR, run what CI runs:

```bash
gofmt -l .              # must print nothing
go vet ./...
go test -race ./...
go mod tidy             # must leave go.mod/go.sum unchanged
```

## The invariants

These are not style preferences. A PR that breaks one of them will be sent back
even if the code is otherwise good, because each is a promise the project makes to
people who point this tool at a production account.

### 1. cloud-echo never writes to AWS

Discovery clients are built by `internal/awsx`, which attaches a middleware
rejecting any operation not on an explicit allow-list. There is no flag to disable
it and there will not be one — see
[ADR-0006](docs/adr/0006-read-only-by-construction.md) and
[ADR-0007](docs/adr/0007-allow-list-over-name-prefix.md).

Do not construct an `aws.Config` yourself. Get one from `awsx.Session`.

### 2. Secret values never leave AWS

`secretsmanager:GetSecretValue` is not on the allow-list. `ssm:GetParameter` is
used only with `WithDecryption=false`. Record a secret's **ARN** — it is a useful
linking signal — and never its value. See
[docs/07-security.md](docs/07-security.md).

Any collector that reads free-form configuration — env vars, command lines,
parameter values — must pass it through `redactValue` / `redactArgs` **before**
building either the spec or `Raw`, and its tests must assert on the serialized
resource that no planted secret survives. Redacting only the normalized spec
leaves the secret in `Raw`, which is written to disk too.

### 3. Every resource carries provenance

The linker's evidence chains bottom out in `Resource.Source`. An edge cloud-echo
cannot justify by pointing at a specific API response field is a bug, not a
heuristic. Keep `Raw` too: it lets new inference rules be developed against
inventories captured months ago, without re-scanning anyone's account.

### 4. Output is deterministic

Collectors run concurrently, so emission order depends on which AWS API answered
first. Anything written to disk goes through `Inventory.Normalize` first. Without
this, `scan --diff` reports noise instead of change.

### 5. A denied call degrades; it does not abort

Real accounts hand out partial permissions. `AccessDenied` on one call becomes a
warning and a partial inventory — and so does an operation the endpoint does not
offer (`UnsupportedOperation`: a feature missing in a region, an emulator's
subset). Emit what was read before a call that can fail this way, or read the
listings independently: one unsupported API must not cost the others. Everything else — a network failure, a cancelled
context — propagates. Reporting a Ctrl-C as a permissions problem sends users to
fix the wrong thing.

## Adding a collector

This workflow is enforced by tests in both directions, so it is worth reading
before you start. Using SQS as the example.

### 1. Declare the operations

In [`internal/awsx/operations.go`](internal/awsx/operations.go), add an entry to
`services`:

```go
{
    SDKID:     "SQS",          // must match middleware.GetServiceID exactly
    IAMPrefix: "sqs",
    Ops:       []string{"GetQueueAttributes", "ListQueues"},
},
```

`SDKID` is the smithy service id, not the IAM prefix — they differ more often than
you would expect (`ApiGatewayV2` vs `apigateway`). If you get it wrong, the guard
blocks every call and tells you so.

Only add an operation you are about to call. The allow-list is the list of
permissions users must grant, and every unnecessary one is a reason for a security
team to say no.

### 2. Regenerate the policy

```bash
go test ./internal/awsx -run TestScannerPolicyMatchesAllowList -update-policy
```

Commit the resulting `policies/cloud-echo-scanner.json`. CI fails if it drifts.

### 3. Write the collector

Implement `discovery.Collector` in `internal/discovery/sqs.go`. Use
[`ecs.go`](internal/discovery/ecs.go) as the reference. Paginate everything — an
account large enough to paginate is exactly the account where a partial graph is
most misleading.

**Resource IDs must be scoped by whatever AWS uses to enforce uniqueness.** ECS
service names are unique only within a cluster, so the ID is
`ecs/<cluster>/<service>`. SQS queue names are unique per account-region, so
`sqs/<name>` is fine. Getting this wrong silently merges two resources into one
node, and the loss surfaces much later as a mysteriously missing edge. See
[docs/09-open-questions.md](docs/09-open-questions.md) Q5.

### 4. Record fixtures

Add `internal/discovery/testdata/accounts/<account>/sqs.json`. The format is a
list of exchanges matched on operation name plus an optional request-body
substring:

```json
[
  { "op": "ListQueues", "response": { "QueueUrls": ["..."] } },
  { "op": "GetQueueAttributes", "match": "orders-events", "response": { "...": "..." } }
]
```

Fixtures are hand-authored or captured from a real account — **if captured, redact
the account id to `123456789012` and strip anything internal.** They are public.

**Never commit a credential-shaped literal, even a fake one.** Secret scanners
(GitHub push protection, GitGuardian, TruffleHog) match on shape, not validity: a
made-up `ghp_…` token or a complete Slack webhook URL can block a push or open a
public "secret leaked" alert. In Go tests, split the literal —
`"gh" + "p_…"` produces the identical runtime value without the contiguous shape.
In JSON fixtures, which cannot concatenate, use a value your redaction rule
catches by key name or entropy but that matches no vendor format
(`/services/T-FAKE/B-FAKE/<token>` rather than a real-shaped webhook).

The fixture transport identifies requests by the service and operation the SDK
puts on the request context, so it works for every protocol. JSON services answer
with a `response` object; Query-protocol services like IAM answer XML through a
`body` string. **Write fixtures from the wire format, not from CLI output.** The CLI prints
output *member* names, which can differ from what travels over HTTP: API Gateway
v1 lists are `item` on the wire and `items` in the CLI, and a fixture copied from
the CLI deserializes to an empty list without an error. When in doubt, the key
names are in the SDK's generated `deserializers.go`.

IAM policy documents are URL-encoded exactly as AWS returns them —
encode with `python3 -c 'import sys,urllib.parse;print(urllib.parse.quote(sys.stdin.read(),safe=""))'`.

Your fixtures must exercise **every** operation you added to the allow-list. The
drift test checks both directions and will fail on an operation that is granted but
never called. If you cannot write a fixture that triggers a permission, that is a
strong signal the permission should not be requested.

Include at least one negative case. `testdata/accounts/partial-denied/` shows the
shape: an API returning `AccessDeniedException` so the degradation path is proven
rather than assumed.

### 5. Register it

Add the collector to `NewRegistry` in
[`internal/discovery/scan.go`](internal/discovery/scan.go). If it needs another
collector's output — as IAM needs the roles ECS and Lambda reference — implement
`DependentCollector` and register it under `dependents`; it runs in a second
phase over a snapshot of the first.

### 6. Update the docs

[docs/02-discovery.md](docs/02-discovery.md) has a per-service table. If your
implementation taught you something the docs get wrong, fix the docs in the same
PR. Code disagreeing with the docs is a bug in one of the two.

## Adding a linker rule

A rule is a `rule_*.go` file in `internal/linker` (see
[docs/03-linker.md](docs/03-linker.md#rule-authoring)). Three things are not
optional:

- **Read specs, never Raw**, through `Each` and the types in
  `internal/inventory/spec`. The golden fixtures carry no Raw.
- **Resolve targets through `Local`**, which refuses ids derived from ARNs in
  another account or region. Names repeat; ids come from names.
- **Ship a negative case** in the hand-written account for its tier or service
  (`testdata/tier{1,2,3}-cases`, `rds-`, `cache-`, `elb-cases`), a named test
  saying why the case matters (`rules_test.go`, `tier2_test.go`, `tier3_test.go`,
  `rds_test.go`, `cache_test.go`, `elb_test.go`), and regenerated goldens:
  `go test ./internal/linker -update`, then read the diff.
- **Never draw more intent than the source states.** Configuration names a
  resource without saying what is done with it: that is `references`, not
  `publish` or `write`. Ambiguity lowers confidence and becomes a finding; it is
  never settled silently.
- **A permission is not a use, and never a state.** Claims from permissions go
  through `Permit` (or `Corroborate`), which cannot change whether an edge is
  enabled, and one source alone stays at `medium`. An absence — "this role cannot
  touch that queue" — is only ever a finding.

## Tests

The bar is that a test must be able to fail for the reason it claims. Before
submitting, try breaking the thing your test covers and confirm it goes red — a
test that passes unconditionally is worse than no test, because it advertises
coverage that does not exist.

Two traps when you do: check that the mutated code **compiles** before reading a
green result as "the test is weak" (a filtered `grep FAIL` hides `build failed`),
and check that your fixture actually contains the input that distinguishes the
behaviour you are pinning. Both have happened in this repo.

Tests that replay fixtures go through the **real** middleware stack, so they
exercise the guard too. Do not stub it out.

## Docs and ADRs

Design docs in `docs/` are living and can be edited. **ADRs are append-only.** To
change a decision, write a new ADR that supersedes or refines the old one and
annotate the old one with a pointer — never rewrite it. The value of an ADR is
knowing what was believed at the time and why. See
[docs/adr/README.md](docs/adr/README.md).

## Commits and PRs

Conventional-ish prefixes: `feat:`, `fix:`, `docs:`, `test:`, `refactor:`,
`spike:`. Scope in parentheses where it helps: `feat(discovery):`.

Explain **why** in the body, not what — the diff already says what. If you
discovered something surprising, or rejected an approach, say so. That reasoning
is the part nobody can reconstruct later.

## CI notes

`govulncheck` runs against the latest stable Go. A finding in the standard library
usually means the toolchain needs a bump, not that your code is wrong.

The materializer's integration tests will eventually need a real Floci container
and will run as a separate slow tier. They do not exist yet.

## License

MIT. By contributing you agree your contributions are licensed under it.
