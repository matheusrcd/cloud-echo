# 03 — The Linker

This is the component the project lives or dies by. Emulation is a solved problem
(Floci). Knowing *what talks to what* is not.

## Edge model

```go
type Edge struct {
    From       string      // resource id
    To         string      // resource id, or "ext/<slug>" for something outside the account
    Kind       EdgeKind    // invoke | publish | consume | read | write | connect | http
    Confidence Confidence  // certain | high | medium | low
    Evidence   []Evidence  // never empty
}

type Evidence struct {
    Rule   string  // "iam.policy-resource"
    Source string  // "iam:GetRolePolicy arn:aws:iam::...:role/orders-api-task"
    Detail string  // "Allow dynamodb:PutItem on arn:aws:dynamodb:...:table/orders"
}
```

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

| Rule | Source | Produces |
| --- | --- | --- |
| `lambda.event-source-mapping` | `ListEventSourceMappings` | SQS/DynamoDB Stream/Kinesis/MSK → Lambda (`consume`) |
| `sns.subscription` | `ListSubscriptionsByTopic` | SNS → SQS/Lambda/HTTP (`publish`) |
| `apigw.integration` | `GetIntegration` | API GW → Lambda (`invoke`) / HTTP (`http`) / VPC Link → ALB |
| `sqs.redrive` | `RedrivePolicy` | queue → DLQ (`publish`) |
| `elbv2.target-group` | ECS service `loadBalancers[]` | ALB → ECS service (`http`) |
| `ecs.image` | task def `containers[].image` | ECS service → ECR repo |
| `ecs.secrets` | task def `containers[].secrets[]` | ECS service → Secrets Manager / SSM (`read`) |
| `lambda.resource-policy` | `GetPolicy` principals | caller → Lambda (`invoke`) |
| `dynamodb.stream` | `StreamSpecification` | table → stream (materialized as one node) |

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

1. **Entrypoints** = nodes with no inbound edges from inside the graph and an
   external trigger: API Gateway stages, ALB listeners, Lambda Function URLs,
   EventBridge schedules, Cognito triggers.
2. **Synchronous flow** = forward traversal from entrypoints across `http`,
   `invoke`, `read`, `write`, `connect` edges only.
3. **Asynchronous branch** = anything first reached by crossing a `publish` or
   `consume` edge. The queue/topic/stream *is* the boundary. Everything downstream
   of it is a side flow, transitively.
4. **Scheduled** = reached only from a cron/rate rule.
5. **Orphan** = reachable from nothing. Usually dead infra; worth reporting as its
   own finding.

Each node gets `flow: entrypoint | sync | async | scheduled | orphan`. This drives:

- default layout in the UI (main flow on the spine, async branches hanging off)
- planner defaults (async consumers are often worth including at depth 1 even when
  the sync path stops at depth 2)
- a genuinely useful report on its own: *"these 4 services are in your request
  path; these 11 are not."*

## Rule authoring

Each rule is an isolated file in `internal/linker/rules/` implementing:

```go
type Rule interface {
    Name() string
    Tier() int
    Apply(inv *inventory.Inventory, g *Graph) error
}
```

Every new rule ships with a golden fixture, **including a negative case**. A rule
that only has positive tests is a rule that will over-link a real account. The
failure mode of this project is not "missed an edge" — it is "produced a hairball
nobody believes."

## Handling wrong inferences

The linker will be wrong. The design assumes it:

- `cloud-echo graph --explain <from> <to>` prints the full evidence chain.
- The UI lets an edge be deleted or added by hand.
- Manual corrections live in `cloud-echo.override.yaml` and **survive re-scans**.
  A user who fixes the same wrong edge twice will stop using the tool.
- `--min-confidence` gates what enters a plan.
