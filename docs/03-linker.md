# 03 — The Linker

This is the component the project lives or dies by. Emulation is a solved problem
(Floci). Knowing *what talks to what* is not.

## Edge model

```go
type Edge struct {
    From       string      // resource id
    To         string      // resource id, or "ext/<host>" for something outside the account
    Kind       EdgeKind    // invoke | publish | consume | read | write | connect | http | references
    Confidence Confidence  // certain | high | medium | low
    Status     Status      // "" (active) | unsettled | disabled
    Evidence   []Evidence  // never empty
}

type Evidence struct {
    Rule   string  // "iam.policy-resource"
    Source string  // "iam:GetRolePolicy arn:aws:iam::...:role/orders-api-task"
    Detail string  // "Allow dynamodb:PutItem on arn:aws:dynamodb:...:table/orders"
}
```

**Edges point in the direction of causality**: from the side that acts to the side
that is acted on or triggered. A service that writes a table points at the table;
a queue that triggers a function through an event source mapping points at the
function. (The first draft never said, which leaves every rule author to pick.)
The planner's slice-from-seed must therefore walk edges both ways — a consumer
needs its source — see [Q16](09-open-questions.md).

**What is a node.** Things that run or hold data: ECS services, Lambda functions,
queues, tables, APIs, and `ext/<host>` for third parties. Task definitions,
roles, policies and clusters are configuration and identity: rules read them
*through* the node that uses them, and they are not nodes themselves.

**Nothing is invented.** A target outside the inventory — another account,
another region, a service without a collector — becomes a `finding`
(`unresolved`), never a node. Ids are derived from names and names repeat across
accounts, so an id derived from an ARN is only trusted when the ARN's account and
region match the scan; otherwise a DLQ in another account would link to a local
queue with the same name, silently. `tier1-cases` pins exactly that trap.

**An edge without evidence is a bug.** The UI shows evidence on hover; the CLI
shows it in `graph --explain`. Users must be able to audit an inference, because
they will not trust a graph they can't check — and if they don't trust the graph,
they won't trust the environment built from it.

## Inference tiers

Rules are grouped by how much they can be trusted. Confidence is not a vibe; it
maps to a policy: **`certain` and `high` are included in the plan automatically;
`medium` is included but flagged for review; `low` is surfaced as a suggestion and
never auto-included.**

---

### Tier 1 — Explicit declarations → `certain`

The account literally states the relationship. No inference.

Implemented rules (M1) are marked ✅; the rest need a collector that does not
exist yet.

| Rule | Source | Produces |
| --- | --- | --- |
| `lambda.event-source-mapping` ✅ | `ListEventSourceMappings` | SQS/DynamoDB Stream → Lambda (`consume`, status from the mapping state); on-failure destination (`publish`). Kinesis/MSK sources are reported unresolved |
| `lambda.dead-letter` ✅ | `DeadLetterConfig` | Lambda → SQS (`publish`) |
| `apigw.entrypoint` ✅ | every API | the API is an entrypoint (trigger) |
| `ecs.load-balancer` ✅ | service `loadBalancers[]` | the service is an entrypoint (trigger); no edge until ELBv2 is collected |
| `sns.subscription` | `ListSubscriptionsByTopic` | SNS → SQS/Lambda/HTTP (`publish`) |
| `apigw.integration` ✅ | route `integration` (v1 `GetResources` embedded, v2 `GetIntegrations`) | API GW → Lambda (`invoke`) / SQS direct (`publish`) / HTTP (`http`) / VPC Link → ALB |
| `apigw.authorizer` ✅ | route `authorizerId` → authorizer `function` | API GW → authorizer Lambda (`invoke`, synchronous — it is in the request path) |
| `apigw.credentials` ✅ | integration `credentials` | the role API GW assumes to call the target — read by Tier 3, where it corroborates the integration |
| `sqs.redrive` ✅ | `RedrivePolicy` | queue → DLQ (`publish`) |
| `elbv2.target-group` | ECS service `loadBalancers[]` | ALB → ECS service (`http`) |
| `ecs.image` | task def `containers[].image` | ECS service → ECR repo |
| `ecs.secrets` | task def `containers[].secrets[]` | ECS service → Secrets Manager / SSM (`read`) |
| `lambda.resource-policy` ✅ | `GetPolicy` principals | **corroborates only** — see below |
| `dynamodb.stream` | `StreamSpecification` | table → stream (materialized as one node) |

An integration marked `templated` (its URI names `${stageVariables.x}`) has no
static target. The rule resolves it **per stage** from that stage's variables, and
emits nothing when a variable is missing rather than guessing.

**A resource policy is a permission, not a use.** The first draft had
`lambda.resource-policy` create caller → function edges; deployment tools leave
stale permissions behind constantly, so it would have drawn edges for callers
that no longer call. It adds evidence to an edge another rule found, reports a
permission nothing uses as a `stale-permission` finding, and makes a function an
entrypoint when the caller is outside the inventory (S3, SNS, EventBridge, an API
in another region), with the reason.

An authorizer that guards no route produces no edge: it runs for nothing. An HTTP
integration through a VPC link targets an internal load balancer, not a third
party, and is not turned into an `ext/` node the gateway would mock.

If Tier 1 covers your architecture, the graph is essentially free. It rarely
covers more than half.

---

### Tier 2 — Configuration value scanning → `high` / `medium` / `low` ✅

One rule, `config.value-scan`. It reads every string the account exposes as
configuration: ECS container env vars and `command`/`entryPoint` (through the
service's task definition), Lambda env vars, and API Gateway stage variables. SSM
parameter values join when the SSM collector exists.

Each value is matched against these patterns, strongest first:

| Pattern | Produces | Confidence |
| --- | --- | --- |
| An ARN, anywhere in the value, of a node in this account and region | `references` | `high` |
| An SQS queue URL — `sqs.<r>.amazonaws.com`, or the legacy `<r>.queue.amazonaws.com` / `queue.amazonaws.com` | `references` | `high` |
| An API invoke URL `https://<id>.execute-api.<r>.amazonaws.com` | `http` to the API | `high` |
| Any other `http(s)://` URL whose host is not AWS, not loopback, and has a domain | `http` to `ext/<host>` | `high` |
| The whole value equal to a table, queue or function name | `references` | `medium` or `low` — below |
| An RDS endpoint — writer, reader, custom, or a member's own — matching a database exactly | `connect` to the database | `high` |
| The ARN of a database's managed master secret (in env, or injected through ECS `secrets[]`) | `connect` to the database | `high` |
| An ElastiCache endpoint — primary, reader, configuration, serverless, or a node's — matching a cache exactly | `connect` to the cache | `high` |
| A load-balancer endpoint, an RDS Proxy, a Function URL, an S3 bucket, another AWS endpoint, a non-HTTP URL outside AWS, a host with no domain | an `unresolved` finding | — |

A command line is read with its flags: `--table orders` and `--table=orders`
carry `table` as their key, and a positional argument yields only what names
itself (a URL, an ARN). A stage variable an integration URI already uses was
interpreted by `apigw.integration` and is not read again; the rest are attributed
to the API, which hands them to its backends.

Two things this tier does that no other tier does:

- **It finds external dependencies.** A URL in an env var that matches nothing in
  the account is a third-party integration, and it becomes a mockable node in the
  blueprint automatically. This is the direct feed into the Echo Gateway. A URL
  whose path was redacted (`hooks.slack.com/services/…/<redacted>`) keeps its
  host, which is all this needs.
- **It tells you the env var name** — every piece of evidence names the variable,
  flag or stage variable it came from — which is exactly what the materializer
  needs in order to rewrite the value to a local address.

**Corrected before implementation** (each pinned by a named test in
`tier2_test.go` and a mutation it kills):

1. **A reference says nothing about intent.** The draft gave confidence per
   pattern and never said what *kind* of edge a pattern produces. `QUEUE_URL` in a
   worker is how it polls as often as how it sends; `TABLE_NAME` is read and write
   alike. Drawing `publish` or `write` would silently pick a winner — the thing the
   guard below forbids. So AWS resources get `references`, which points from the
   holder to the target and claims no more. For flow it is decided by its target:
   a reference to a queue is an async boundary (whatever the workload does with
   the queue, the queue decouples it), anything else is synchronous.
2. **A reference folds into the edge that states its intent.** When Tier 1 (or,
   later, Tier 3) draws a typed edge between the same pair, a `references` edge of
   `medium` or better becomes evidence on it and raises its confidence to the
   higher of the two: a Lambda's `DLQ_URL` is evidence for its dead-letter `publish`
   edge, not a second edge beside it. Only the same direction absorbs — a consumer
   holding its own queue's URL still references it — and a `low` candidate never
   does, or an ambiguous name would read as corroboration.
3. **AWS endpoints are never third parties.** "Any other absolute URL" would have
   made `ext/` nodes of `sqs.us-east-1.amazonaws.com`, an RDS endpoint, and
   `http://localhost:2773` (the secrets-cache extension) — mocks of AWS itself and
   of the workload's own sidecar. Hosts under `amazonaws.com`, `on.aws` and
   `api.aws` are resolved to a node or reported; loopback and link-local hosts are
   ignored; a host with no domain (`http://search:9200`) is an internal name —
   Service Connect, Cloud Map, a container link — and is reported, because made
   external it would have the gateway mock the user's own service.
4. **The key name breaks a tie, and says so.** `TABLE_NAME=orders` in an account
   with a table *and* a queue called `orders` — an ordinary account, and exactly
   what the real validation account holds — means the table. The candidate whose
   type the key names gets `medium` with the reason stated; the others stay `low`
   candidates, and an `ambiguous` finding records the choice. Without a hint every
   candidate is `low`. A key naming a type no candidate has (`QUEUE_NAME=x` where
   only a table is called `x`) lowers them all: the queue it means may simply be
   outside the scan. A bare name is never more than `medium`.
5. **"Generic" is defined.** A name without a separator or a digit (`orders`,
   `info`, `jobs`) or shorter than four characters matches by coincidence as easily
   as by design: alone and unhinted it is `low`.
6. **The namesake trap applies here too.** ARNs and queue URLs pass the same
   account and region check as Tier 1 — a queue URL in another account whose name
   matches a local queue is reported, never linked. The real account carries
   exactly that case.
7. **Blind spots are findings.** A service whose task definition is not in the
   inventory, or a function whose environment could not be decrypted, is an
   `unscanned` finding — so no references is never mistaken for "names nothing".
   Log driver options are not scanned: they configure the agent, not the code.

Guard: an env var value scanner will produce false positives on generic names.
Ambiguity must *downgrade* confidence, never silently pick a winner. When a bare
name matches more than one resource, emit `low`-confidence candidates for all of
them and let the user choose.

**Known gaps.** A custom domain in front of one of the account's own APIs or load
balancers (`https://api.company.com`) is indistinguishable from a third party
until domain mappings are collected ([Q15](09-open-questions.md)). An internal
hostname with a domain (`inventory.internal.corp`) is treated as a third party,
as [06-echo-gateway.md](06-echo-gateway.md) intends for hosts that match nothing.

---

### Tier 3 — IAM policy analysis → `medium` / `low`, `high` when Tier 2 agrees ✅

One rule, `iam.policy-resource`. For every role in the inventory it finds the
workloads running as it — Lambda execution roles, ECS **task** roles (through the
task definition, to every service running it), and the roles API Gateway
integrations assume — reads its inline policies, attached policies and permissions
boundary, and evaluates what it allows on every queue, table and function in the
inventory, the way IAM does: an `Allow` in the role's policies, no unconditional
`Deny`, and an `Allow` in the boundary if there is one. `Action`/`NotAction`,
`Resource`/`NotResource`, `*` and `?` are all honoured.

```
sqs:SendMessage                                 → publish   workload → queue
sqs:ReceiveMessage                              → consume   queue → workload
dynamodb:GetItem, BatchGetItem, Query, Scan, …  → read      workload → table   (Query/Scan also on its indexes)
dynamodb:PutItem, UpdateItem, DeleteItem, …     → write     workload → table
dynamodb:GetRecords on the stream               → consume   table → workload
lambda:InvokeFunction                           → invoke    workload → function
```

This tier is uniquely valuable because it gives **direction and intent** — Tier 2
tells you a service knows a table's name, Tier 3 tells you it writes to it. That
distinction drives `data.mode` defaults and the sync/async classification. Its
typed edges absorb the Tier-2 `references` between the same pair, and the one
reverse case — a worker whose role only receives from the queue it names — folds
into the consume edge ([Q17](09-open-questions.md), closed).

**Corrected before implementation** (each pinned by a named test in
`tier3_test.go` and a mutation it kills):

1. **A permission is not a use.** The draft rated this tier `high`. An identity
   policy is a permission exactly as a resource policy is — the reason
   `lambda.resource-policy` only corroborates — and deployment tools grant
   generously and rarely take grants back. A role alone makes an edge `medium` at
   most: included, flagged. **Two independent sources agreeing is what `high`
   means**: when the configuration names the table (Tier 2) and the role may
   write it (Tier 3), each already at `medium`, the edge is `high`. The same rule
   applies wherever edges from different rules meet.
2. **Intent comes from named actions.** The verb table above says nothing about
   `sqs:*` on one queue, which permits sending *and* receiving. Drawing both would
   invent a consumer; drawing either would pick. A grant whose action is
   service-wide (`*`, `sqs:*`, `NotAction`) states no intent and becomes a
   `references` edge.
3. **A permission never changes an edge's state.** Edges merge by "most active
   status", and the role behind a disabled event source mapping can still receive
   from its queue — the real validation account has exactly this. Tier-3 claims
   go through `Context.Permit`: they create an edge when none exists and otherwise
   add evidence and confidence, never status.
4. **Broad is about the name, not only `Resource: "*"`.** `table/*` reaches every
   table as surely as `*` does. Any grant whose resource-name segment is `*`
   draws nothing and is reported once per workload and service as `broad-access`
   — a finding, since nodes carry no annotations. AWS managed policies fall out of
   this naturally: they cannot name your resources.
5. **A pattern reaching several resources is a candidate, not a fact.** The draft
   expanded wildcard ARNs at `medium`; `jobs-*` matching seven queues would be
   seven edges from one line. A pattern matching one node is that node (`medium`);
   matching several, each is `low` — until the configuration names one of them.
6. **What cancels a grant is said.** A grant an unconditional `Deny` or the
   boundary cancels is a `blocked` finding, so the missing edge is explainable. A
   conditional `Deny` may not apply and cancels nothing. Guardrails — `dynamodb:*`
   except `Delete*`, a boundary trimming `sqs:*` on `*` — are not reported.
7. **Absence is a finding, never an edge change.** A reference the holder's role
   cannot act on — the role read in full, no grant on the target (a broad one
   counts), no queue or function policy naming the role — is an `unpermitted`
   finding: dead configuration, or access cloud-echo does not see. It is what
   settles Tier 2's ambiguity: `TABLE_NAME` names a table and a queue, and the role
   writes the table and cannot touch the queue.
8. **"Could not read" must not look like "grants nothing".** The collector
   recorded a refused `ListRolePolicies` only as a scan warning, so a partly read
   role looked like an empty one. The role now carries `unread`; with anything
   unread, or an attached policy missing, nothing is concluded from absence, and
   the workload gets an `unscanned` finding. A boundary that cannot be read lowers
   the role's edges to `low` — its whole purpose is to restrict. A workload whose
   role is not in the inventory at all is `unscanned` too.
9. **Lambda receives through its mappings.** Lambda polls a queue or a stream for
   an event source mapping with the function's own role. A function that may
   receive from a queue no mapping connects it to is most likely holding a
   leftover grant: `low`.
10. **An API's role only corroborates.** An API's behaviour is its routes, which
    Tier 1 reads in full; the role an integration assumes confirms them and draws
    nothing new.

Also: only ECS task roles and Lambda execution roles feed this tier (ECS execution
roles describe the agent and are not collected); a grant naming one exact queue,
table or function outside the scan — another account, the namesake trap again,
or deleted — is `unresolved`; grants on services without nodes (logs, X-Ray, KMS,
S3, SNS, Secrets Manager) are the infrastructure every role carries and are not
reported until each service's collector exists.

#### Databases (RDS)

What linking a database adds to the tiers above, each pinned by a named test in
`rds_test.go` and a mutation it kills:

- **An endpoint matches exactly or not at all.** `<name>.<suffix>.<region>.rds.amazonaws.com`:
  the suffix belongs to the account and region. `ce-test-db.cluster-abc123…` is
  another account's `ce-test-db` — the real validation account's ECS service
  carries exactly that — and matching on the name would link a service to a
  database it cannot reach. A near miss says so: "a namesake, not this database".
  RDS Proxy endpoints and other regions are reported as such.
- **A database endpoint states its verb.** It is only good for connecting, so
  Tier 2 draws `connect` — as a URL draws `http` — not `references`.
- **Every endpoint of a cluster is the cluster**: writer, reader, custom, and
  each member's own.
- **The managed master secret is a link.** A workload handed a database's
  password — its ARN in an env var, injected through `secrets[]` (even with a
  `:password::` JSON-key suffix), or read with `secretsmanager:GetSecretValue`
  (Tier 3) — connects to that database. Other secrets say nothing until the
  Secrets Manager collector exists.
- **The Data API is IAM**: `rds-data:ExecuteStatement` on a cluster's ARN is a
  Tier-3 `connect`.
- **A database is not IAM-gated.** It takes a password over a route; a role with
  no grant on it proves nothing, so a reference to one is never `unpermitted`.
  And a `GetSecretValue` on `*` is broad access to *secrets*: `broad-access` is
  named by the grant's service, not the target's.
- Database names (`DB_NAME=orders`) and bare identifiers are never matched: names
  repeat everywhere, and identifiers are not how applications reach a database.
- Gap: IAM database authentication (`rds-db:connect`) names a resource id and a
  database user rather than an ARN, and is not read yet.

#### Caches (ElastiCache)

The same decisions as databases, pinned by `cache_test.go` and a mutation each:
an endpoint of any shape matches its cache exactly or not at all, and a label
naming a cache of this scan beside another account's suffix is reported as a
namesake — including a serverless host, whose first label is `<name>-<suffix>`;
the edge is `connect`; `elasticache:Connect` (IAM authentication) is a Tier-3
`connect` on the cache's ARN; and a cache is not IAM-gated, so a reference to one
is never `unpermitted`. A serverless cache's reader shares its writer's host, so
the host names the cache and not the role.

**Known gaps.** Service control policies and session policies are not collected;
a DynamoDB resource policy is not collected (so `unpermitted` names it as a
possibility); a queue or function policy that grants a role is read only to hold
back `unpermitted`, not to draw edges. An integration whose target is withheld in
a mapping template stays unresolved even when its role names the target — the
role says what it may reach, not which call the template makes.

---

### Tier 4 — Network reachability → corroboration only

Security group ingress/egress plus VPC/subnet placement. On its own this is far
too weak to create an edge (a shared SG links a dozen unrelated things). It is
used only to **upgrade or downgrade** an existing hypothesis:

- ECS service SG can reach RDS SG on 5432 **and** Tier 2/3 found a hint →
  upgrade `medium` → `high`.
- Tier 2 found a hint but the SGs make the connection impossible → downgrade and
  flag as "config references it but cannot reach it" (frequently a real finding
  about dead config).

---

### Tier 5 — Runtime observation → out of scope for v1

X-Ray service graphs, VPC Flow Logs, CloudWatch Logs Insights. Highest fidelity by
far — it is *observed* rather than inferred. Also: needs extra permissions, costs
money, requires instrumentation to already exist, and is slow.

Explicitly deferred. The `Evidence.Rule` namespace reserves `xray.*` and `logs.*`
so adding it later is additive.

## Flow classification

The user question "what runs on the side of the main flow" is answered
structurally, not heuristically:

1. **Entrypoints** = nodes with an external trigger: API Gateway APIs, services
   behind a load balancer, functions a caller outside the inventory may invoke,
   Function URLs (later: EventBridge schedules, Cognito triggers). *Corrected
   from "no inbound edges and an external trigger": a service behind an ALB that
   another service also calls is still reachable from outside.*
2. **Sync** = reachable from an entrypoint through a path of **only**
   synchronous edges (`http`, `invoke`, `read`, `write`, `connect`).
3. **Async** = reachable, but only across a `publish` or `consume` edge. The
   queue/topic/stream is the boundary.
4. **Scheduled** = reached only from a schedule (needs EventBridge).
5. **Unreached** = no path from an entrypoint found.

**Sync takes precedence**, and nothing depends on traversal order. The draft said
async was anything "first reached" across a queue, which is order-dependent: a
table written by the request path and by a worker would flip between runs.

**Unreached, not orphan.** cloud-echo cannot tell dead infrastructure from a
relationship it failed to infer. At Tier 1 a queue whose producer writes through
the SDK — the common case — has no inbound edge, so its whole pipeline is
unreached; on the real validation account that was 23 of 31 nodes, all alive.
"Orphan" would assert they are dead. Tier 2 brought it to 18, each one explained
(services with no load balancer, a queue only low candidates reach) — see the
[findings](spikes/m1-real-account-findings.md#linker-round-tier-2). Tier 3 changed
none of those flows there; it changed what the edges mean
([Tier-3 round](spikes/m1-real-account-findings.md#linker-round-tier-3)).

Disabled edges (a disabled mapping) are recorded and not followed. Unsettled
edges (a mapping caught mid-update) are followed and flagged. **Low-confidence
edges are not followed either**: `low` is a suggestion the user has not accepted,
and a flow resting on one would put a queue on the request path because
`LOG_LEVEL=info` happens to be its name.

Each node gets `flow: entrypoint | sync | async | scheduled | unreached`. This
drives:

- default layout in the UI (main flow on the spine, async branches hanging off)
- planner defaults (async consumers are often worth including at depth 1 even when
  the sync path stops at depth 2)
- a genuinely useful report on its own: *"these 4 services are in your request
  path; these 11 are not."*

## Rule authoring

Each rule is a `rule_*.go` file in `internal/linker` implementing:

```go
type Rule interface {
    Name() string
    Tier() int
    Apply(c *Context)
}
```

`Context` is the only way a rule touches the graph: `Each` (typed specs) and
`lookup` (one resource by id), `Edge` (drops to a finding when either end is not
a node), `Local` (the account/region check), `External`, `Trigger`,
`Corroborate` (evidence, never an edge), `Permit` (an edge when none exists,
otherwise evidence and confidence — never status), `Unresolved` and `Finding`. Rules read **specs only, never Raw** —
the golden fixtures carry no Raw, so a rule that reached for it would find
nothing.

Findings have seven kinds: `unresolved` (a reference to something outside the
inventory or without a node), `stale-permission`, `ambiguous` (a name that fits
several resources), `unscanned` (configuration or a role that could not be read),
`broad-access` (a grant reaching every resource of a service), `blocked` (a grant
a `Deny` or the boundary cancels), `unpermitted` (a reference the role cannot act
on). The text
output groups identical findings, so twelve services sharing one task definition
report one ElastiCache endpoint once; `graph.json` keeps every one.

Every rule ships with a golden case **and a negative case**. Seven golden
accounts in `internal/linker/testdata`:

| Account | What it is |
| --- | --- |
| `orders` | exactly what discovery produces from its own fixtures; a test in discovery fails if it drifts |
| `tier1-cases` | hand-written, one scenario per Tier-1 rule and per trap |
| `tier2-cases` | hand-written, one scenario per Tier-2 pattern and per trap; `tier2_test.go` names each |
| `tier3-cases` | hand-written, one scenario per Tier-3 decision and per trap; `tier3_test.go` names each |
| `rds-cases` | hand-written, one scenario per database-linking decision and trap; `rds_test.go` names each |
| `cache-cases` | hand-written, one scenario per cache endpoint shape and trap; `cache_test.go` names each |
| `real-m1` | a real account's inventory, sanitized by `spikes/m1/sanitize-inventory.py` |

Goldens pin the output; named tests in `rules_test.go` say why each behaviour
matters, so a golden diff cannot be approved without reading what it breaks. A
rule that only has positive tests is a rule that will over-link a real account.
The failure mode of this project is not "missed an edge" — it is "produced a
hairball nobody believes."

## Handling wrong inferences

The linker will be wrong. The design assumes it:

- `cloud-echo graph --explain <from> <to>` prints the full evidence chain ✅.
- The UI lets an edge be deleted or added by hand.
- Manual corrections live in `cloud-echo.override.yaml` and **survive re-scans**.
  A user who fixes the same wrong edge twice will stop using the tool.
- `--min-confidence` gates what enters a plan.
