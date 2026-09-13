# 03 — The Linker

This is the component the project lives or dies by. Emulation is a solved problem
(Floci). Knowing *what talks to what* is not.

## Edge model

```go
type Edge struct {
    From       string      // resource id
    To         string      // resource id, or "ext/<host>" for something outside the account
    Kind       EdgeKind    // invoke | publish | consume | read | write | connect | http
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
| `apigw.credentials` | integration `credentials` | API GW assumes a role to call the target; feeds Tier 3 |
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

### Tier 2 — Configuration value scanning → `high` / `medium`

Scan every string the account exposes as configuration: ECS container env vars and
`command`/`entrypoint`, Lambda env vars, non-secure SSM parameter values, API GW
stage variables.

Match against these patterns, in order:

| Pattern | Confidence | Note |
| --- | --- | --- |
| Full ARN `arn:aws:...` matching an inventory resource | `high` | strongest Tier-2 signal |
| SQS queue URL `https://sqs.<region>.amazonaws.com/<acct>/<name>` | `high` | |
| RDS endpoint `*.rds.amazonaws.com` matching an instance endpoint | `high` | |
| ElastiCache endpoint `*.cache.amazonaws.com` | `high` | |
| API GW invoke URL `https://<id>.execute-api.<region>.amazonaws.com` | `high` | |
| Cloud Map / internal DNS matching a registered service | `high` | |
| Bare string exactly equal to a **unique** table/queue/topic name | `medium` | e.g. `TABLE_NAME=orders`. Downgrade to `low` if the name is short, generic, or ambiguous across types |
| Any other absolute `http(s)://` URL | `high` (as `ext/*`) | **this is how third-party integrations are discovered** |

Two things this tier does that no other tier does:

- **It finds external dependencies.** A URL in an env var that matches nothing in
  the account is a third-party integration, and it becomes a mockable node in the
  blueprint automatically. This is the direct feed into the Echo Gateway.
- **It tells you the env var name**, which is exactly what the materializer needs
  in order to rewrite the value to a local address.

Guard: an env var value scanner will produce false positives on generic names.
Ambiguity must *downgrade* confidence, never silently pick a winner. When a bare
name matches more than one resource, emit `low`-confidence candidates for all of
them and let the user choose.

---

### Tier 3 — IAM policy analysis → `high` / `medium`

Resolve the ECS task role or Lambda execution role, expand attached managed
policies and inline policies, and for each `Allow` statement:

```
action prefix  → service           (dynamodb:PutItem → dynamodb)
action verb    → intent            (Put/Update/Delete/Batch* → write, Get/Query/Scan → read)
Resource ARNs  → inventory match   (arn:...:table/orders → ddb/orders)
```

This tier is uniquely valuable because it gives **direction and intent** — Tier 2
tells you a service knows a table's name, Tier 3 tells you it writes to it. That
distinction drives `data.mode` defaults and the sync/async classification.

Handling of the messy parts:

- `Resource: "*"` → do **not** emit concrete edges. Emit one `low`-confidence
  "has broad access to `<service>`" annotation on the node. Otherwise a single
  over-permissive role links everything to everything and the graph is worthless.
- Wildcard ARNs (`arn:aws:dynamodb:*:*:table/orders-*`) → expand against the
  inventory, `medium` confidence.
- Explicit `Deny` statements → suppress the edge.
- **Permissions boundary** → intersect. Effective permission is what the role's
  policies allow *and* the boundary allows; a policy granting `dynamodb:*` under
  a boundary that only permits SQS writes nothing to DynamoDB. The collector
  records the boundary document alongside the role for exactly this.
- Only application identities feed this tier: ECS **task** roles and Lambda
  execution roles. ECS execution roles describe the agent, not the code, and are
  not collected.
- Managed AWS policies (`AmazonDynamoDBFullAccess`) → treated as `Resource: "*"`,
  i.e. annotation only.

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
unreached; on the real validation account that was 22 of 30 nodes, all alive.
"Orphan" would assert they are dead.

Disabled edges (a disabled mapping) are recorded and not followed. Unsettled
edges (a mapping caught mid-update) are followed and flagged.

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

`Context` is the only way a rule touches the graph: `Each` (typed specs),
`Edge` (drops to a finding when either end is not a node), `Local` (the
account/region check), `External`, `Trigger`, `Corroborate`, `Unresolved`. Rules
read **specs only, never Raw** — the golden fixtures carry no Raw, so a rule that
reached for it would find nothing.

Every rule ships with a golden case **and a negative case**. Three golden
accounts in `internal/linker/testdata`:

| Account | What it is |
| --- | --- |
| `orders` | exactly what discovery produces from its own fixtures; a test in discovery fails if it drifts |
| `tier1-cases` | hand-written, one scenario per rule and per trap |
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
