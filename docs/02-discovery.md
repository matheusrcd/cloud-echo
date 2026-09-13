# 02 — Discovery (the scanner)

## Contract

```go
type Collector interface {
    // Service returns the SDK service id, e.g. "ECS". It must match the
    // SDKID in the awsx allow-list.
    Service() string

    // Collect emits resources on out. It must be side-effect free.
    Collect(ctx context.Context, s *awsx.Session, out Emitter) error
}

// Emitter is how a collector reports what it found and what it could not see.
type Emitter interface {
    Emit(inventory.Resource)
    Warn(inventory.Warning)
}
```

Collectors that depend on what others found implement `DependentCollector`
instead, and run in a **second phase** over a snapshot of the first:

```go
type DependentCollector interface {
    Service() string
    CollectFrom(ctx context.Context, s *awsx.Session, prior []inventory.Resource, out Emitter) error
}
```

IAM is the reason it exists: cloud-echo reads the roles that collected workloads
assume, and cannot know which roles those are until the workloads have been read.

`Emitter` rather than a bare `chan<- Resource`: rule 3 below requires a collector
to report *partial* failure, and a channel of resources has nowhere to put "I was
denied `DescribeTaskDefinition` on this one." Warnings that travel out-of-band
tend to get dropped.

Rules every collector obeys:

1. **Read-only.** Only `Describe*`, `List*`, `Get*`, `BatchGet*`. Enforced at the
   SDK middleware level, not by convention — see [07-security.md](07-security.md).
2. **Fully paginated.** Never trust a first page.
3. **Degrades, never fails.** An `AccessDenied` on one service produces a warning
   and a partial inventory, not an aborted scan. Real accounts have partial
   permissions; a scanner that dies on the first denial is useless.
4. **Records provenance.** Every resource keeps the API call and response path it
   came from. The linker's evidence chain depends on this.
5. **Redacts before it records.** Free-form configuration (env vars, command
   lines) passes through `redactValue`/`redactArgs` before `Spec` *or* `Raw` is
   built. Tests assert on the serialized resource, because `Raw` reaches disk too.

## Normalized resource model

```go
type Resource struct {
    ID        string            // stable local id, e.g. "ecs/orders-api"
    Type      string            // "ecs.service", "dynamodb.table", ...
    ARN       string
    Region    string
    AccountID string
    Name      string
    Tags      map[string]string
    Spec      json.RawMessage   // type-specific, normalized
    Raw       json.RawMessage   // API response, secret-shaped values redacted (evidence + future rules)
    Source    Provenance        // {api: "ecs:DescribeServices", collectedAt: ...}
}
```

`Spec` is typed by the structs in
[`internal/inventory/spec`](../internal/inventory/spec/spec.go), together with the
id scheme. That package is the contract between discovery and every later stage;
the linker reads specs through it and never touches Raw.

Keeping `Raw` matters: new linker heuristics can be developed and tested against
old inventories without re-scanning. The one change made to it is redaction of
secret-shaped configuration values, applied before `Spec` or `Raw` is built — see
[07-security.md](07-security.md), Guarantee 2.

### Resource IDs

IDs are `<service>/<scope...>/<name>`, scoped by whatever AWS actually uses to
make the name unique:

| | |
| --- | --- |
| `ecs/cluster/main` | cluster |
| `ecs/main/orders-api` | service — **scoped by cluster** |
| `ecs/taskdef/orders-api:41` | task definition, family + revision |
| `sqs/orders-events` | queue — unique per account-region, no scope needed |
| `ddb/orders` | table — unique per account-region |
| `lambda/order-processor` | function — unique per account-region |
| `lambda/esm/<uuid>` | event source mapping — its own resource, see below |
| `apigw/a1b2c3d4e5` | API — the **API id**, not the name (names are not unique) |
| `iam/role/orders-api-task` | role — unique per account **regardless of path** |
| `iam/policy/orders-rw` | customer-managed policy |
| `iam/aws-policy/AmazonSQSFullAccess` | AWS-managed policy — a customer policy may reuse the name, so they must not share an id |

ECS service names are only unique *within a cluster*. A bare `ecs/orders-api`
would silently collapse two different services in any account that reuses names
across clusters, and the loss would surface much later as a node mysteriously
missing from the graph. `TestECSDistinguishesSameNamedServicesAcrossClusters`
covers it, and `Inventory.Write` refuses to serialize a duplicate ID at all
rather than letting one resource overwrite another.

Note this makes the doc-04 examples (`id: ecs/orders-api`) shorthand rather than
literal. See [09-open-questions.md](09-open-questions.md) Q5 for the general rule.

## Per-service calls (v1)

### Primary services

The allow-list in [`internal/awsx/operations.go`](../internal/awsx/operations.go)
is authoritative; this table is the reasoning behind it. A per-collector test
asserts the two match **in both directions** — an operation granted but never
called is a permission we ask users for and waste, which is its own kind of bug.

| Service | Calls | Key fields for linking |
| --- | --- | --- |
| **ECS** ✅ | `ListClusters`, `DescribeClusters`, `ListServices`, `DescribeServices`, `DescribeTaskDefinition` | container `image`, `environment`, `secrets`, `portMappings`, `command`, `taskRoleArn`, `executionRoleArn`, `networkConfiguration`, `loadBalancers`, `serviceRegistries` |
| **Lambda** ✅ | `ListFunctions`, `ListEventSourceMappings`, `GetPolicy` | `Environment.Variables`, `Role`, `DeadLetterConfig`, event source ARNs, resource-policy principals |
| **SQS** ✅ | `ListQueues`, `GetQueueAttributes`, `ListQueueTags` | `RedrivePolicy` (→ DLQ), `VisibilityTimeout`, `Policy` (→ who can send), `FifoQueue` |
| **DynamoDB** ✅ | `ListTables`, `DescribeTable`, `DescribeTimeToLive`, `ListTagsOfResource` | key schema, GSIs/LSIs, `StreamSpecification` |
| **RDS** ✅ | `DescribeDBInstances`, `DescribeDBClusters` | endpoints (writer, reader, custom, members'), `Port`, `Engine`/`EngineVersion`, the managed master secret's ARN, `DbiResourceId`, subnet group and security groups, Data API, Serverless v2 range |
| **API Gateway v1** ✅ | `GetRestApis`, `GetResources` (`embed=methods`), `GetStages`, `GetAuthorizers` | integration `uri`, `type`, `credentials`, `connectionId`; authorizer function; stage variables |
| **API Gateway v2** ✅ | `GetApis`, `GetRoutes`, `GetIntegrations`, `GetStages`, `GetAuthorizers` | integration `uri`/subtype + `QueueUrl`; JWT issuer; authorizer function |
| **ElastiCache** | `DescribeReplicationGroups` (Redis/Valkey), `DescribeCacheClusters` (memcached only) | `Engine`, `EngineVersion`, endpoint, port |

> **Tags come from `Include`, not a second call.** Every ECS `Describe*` accepts
> an `Include: [TAGS]` parameter, so `ListTagsForResource` is not needed and is
> not on the allow-list. Worth checking per service before adding a tag call.

> **`ListTasks`/`DescribeTasks` are not collected in v1.** The task *definition*
> attached to a service is what the local environment reproduces; the running
> tasks add nothing the materializer uses. They were on the original list, and
> asking for a permission we never exercise is exactly what the drift test exists
> to prevent.

> **`DescribeContinuousBackups` was dropped from the DynamoDB list.** Point-in-time
> recovery has no meaning for a local emulated table, so the permission would buy
> nothing. The same reasoning removed `ecs:ListTasks`: if a permission cannot be
> justified by something the materializer uses, it should not be requested.

> **Two DynamoDB calls are load-bearing and easy to miss.** `DescribeTable`
> returns the key schema and the attribute *types* as two separate lists, and a
> local table built from either half alone accepts writes in production and
> rejects them locally. TTL is not in `DescribeTable` at all — it needs
> `DescribeTimeToLive`, and without it a local table keeps rows the real one
> would have expired.

> **API Gateway, checked against a real account before the collector was
> written:**
>
> - `GetResources` with `embed=methods` returns every method *with its
>   integration*, so the per-method `GetMethod`/`GetIntegration` the table once
>   listed are gone — two calls per method saved on a control plane that
>   throttles at a few requests per second.
> - API names are **not unique** (`create-rest-api` with an existing name makes a
>   second API), so ids are API ids. v1 and v2 ids share one namespace — the
>   `{id}.execute-api` hostname.
> - v2 has the SQS trap: no `NextToken` unless `MaxResults` is sent.
> - A REST API's resource policy arrives as the *body of a JSON string literal*
>   (`{\"Version\"…\/*…}`); `strconv.Unquote` cannot read `\/`, so it is decoded
>   as a JSON string.
> - v2 service integrations (SQS-SendMessage) have no URI; the queue is in
>   `requestParameters.QueueUrl`.
> - Stage variables and literal parameter mappings are redacted; mapping
>   expressions (`method.request.header.Authorization`, `$request.body`) are
>   references and kept. Mapping templates are free text and withheld entirely —
>   only their content types are recorded.
> - IAM for all of it is `apigateway:GET`, scoped to API definitions so it cannot
>   reach API key values — [ADR-0008](adr/0008-scope-coarse-iam-actions.md).
> - Known gap: routes are the API's *current definition*; a REST stage serves a
>   deployment snapshot that can lag behind undeployed edits.

> **RDS, read against a real account before the collector was written:**
>
> - **Two calls, not three.** `DescribeDBInstances` embeds each instance's subnet
>   group (VPC and subnets), so `DescribeDBSubnetGroups` was dropped; clusters list
>   their custom endpoints, so `DescribeDBClusterEndpoints` is not needed either.
>   Tags arrive in both responses.
> - **The node for an Aurora or Multi-AZ DB cluster is the cluster.** Applications
>   connect to its writer, reader or custom endpoints; its instances are recorded
>   as the cluster's `members`, their own endpoints kept, never as databases of
>   their own. Instance and cluster identifiers are separate namespaces in RDS, so
>   the ids are `rds/<instance>` and `rds/cluster/<cluster>`.
> - **The RDS API also serves DocumentDB and Neptune.** A Neptune graph database
>   comes back from `DescribeDBInstances` beside a Postgres one; those engines are
>   reported (`out-of-scope`) and skipped.
> - **An endpoint's suffix belongs to the account and region**
>   (`<name>.cluster-<suffix>.<region>.rds.amazonaws.com`), which is what lets the
>   linker tell this account's `orders-db` from another account's.
> - **Express clusters live outside any VPC.** `VPCNetworkingEnabled` is false,
>   `InternetAccessGatewayEnabled` true, no subnet group, no security groups —
>   recorded, since it changes both network reachability (Tier 4) and exposure.
> - The master password is never returned and never recorded; the ARN of the
>   secret RDS manages for it is, because a workload holding that ARN connects.
> - Known gaps: RDS Proxy endpoints (`DescribeDBProxies`) and Aurora global
>   databases are not collected.

> **`ListQueues` needs `MaxResults` to paginate at all.** Without it AWS returns up
> to 1000 queues and no `NextToken` — silent truncation, confirmed against the
> real API. The collector always sends it.

> **Mapping state is tri-state.** `Creating` and `Updating` say nothing about
> whether a mapping is enabled; a live mapping scanned mid-update read as disabled
> under a two-state rule. `enabled` is `null` there, with `transitional: true`.

> **Lambda asks for three permissions, not six.** `ListFunctions` already
> returns environment, role and runtime, so `GetFunctionConfiguration` is
> redundant. `GetFunction` is deferred to M3, when a container image URI is
> actually needed — and its response carries `Code.Location`, a presigned URL to
> download the function's source, which a topology scan has no business holding.
> `ListFunctionUrlConfigs` and `GetFunctionCodeSigningConfig` feed nothing the
> linker or materializer uses yet.
>
> Event source mappings are emitted as **their own resources**: they are
> independent AWS resources listed account-wide, a function can have several, and
> they often target an alias. Keeping them separate also keeps provenance honest —
> the evidence for a queue → function edge is `ListEventSourceMappings`, not the
> call that listed the function.
>
> **Known gap:** `ListFunctions` returns `$LATEST`. If production goes through an
> alias pinned to an older version, that version's environment can differ. The
> mapping's `qualifier` is recorded so the gap is at least visible.

> **ElastiCache is two APIs, not one.** Redis and Valkey clusters are *only*
> visible through `DescribeReplicationGroups`; `DescribeCacheClusters` covers
> memcached. M0 confirmed Floci enforces the same split as modern AWS
> (`CreateCacheCluster` rejects Redis/Valkey with *"Engine must be 'memcached'"*).
> A collector that reads only `DescribeCacheClusters` will silently miss every
> Redis cluster in the account.

### Supporting services

| Service | Calls | Why |
| --- | --- | --- |
| **IAM** ✅ | `GetRole`, `ListRolePolicies`, `GetRolePolicy`, `ListAttachedRolePolicies`, `GetPolicy`, `GetPolicyVersion` | Tier-3 permission-based edge inference — the highest-value heuristic |
| **ECR** | `DescribeRepositories`, `DescribeImages` | resolve image tag → digest so the local env is pinned |
| **SNS** | `ListTopics`, `GetTopicAttributes`, `ListSubscriptionsByTopic` | fan-out edges |
| **Secrets Manager** | `ListSecrets`, `DescribeSecret` | **never `GetSecretValue`** by default |
| **SSM** | `DescribeParameters`, `GetParameters` (String/StringList only) | config values are a rich linking signal; SecureString is skipped |
| **EC2** | `DescribeVpcs`, `DescribeSubnets`, `DescribeSecurityGroups` | Tier-4 reachability corroboration |
| **ELBv2** | `DescribeLoadBalancers`, `DescribeTargetGroups`, `DescribeListeners`, `DescribeRules` | API GW / ALB → ECS path |
| **CloudWatch Logs** | `DescribeLogGroups` | map workloads to log groups (used later for runtime observation) |

> **IAM reads only the roles workloads assume.** No `ListRoles`, no
> `ListPolicies`: a real account holds hundreds of SSO, service-linked and
> bootstrap roles the linker would only have to ignore. The roles read are ECS
> *task* roles and Lambda execution roles — the identities application code runs
> as. ECS *execution* roles are skipped on purpose: they belong to the ECS agent,
> which uses them to pull images and inject `secrets[]`, and reading them as the
> application's permissions would make every service appear to read every secret
> the agent fetches for it.
>
> Also recorded: the **permissions boundary**, because effective permission is
> the intersection of the role's policies with it; roles that are referenced but
> **no longer exist** (a `dangling-reference` warning — that workload cannot
> start); and roles in **another account**, which are reported and never looked
> up, since `GetRole` takes a name and would silently return a different,
> same-named local role. Every policy document IAM returns is URL-encoded; the SDK
> does not decode it, the collector does. And what it could not read — a refused
> `ListRolePolicies`, one inline policy, an unparseable document — is recorded on
> the role as `unread`, not only as a scan warning: otherwise a partly read role
> looks like one that grants nothing, and Tier 3 would conclude from a blind spot.

## Scan scope and cost

- **Regions**: single region in v1 (`--region`, defaults to the profile's).
  Multi-region is a v2 concern and mostly a fan-out change.
- **Filters**: `--include-tag`, `--exclude-tag`, `--vpc`, `--name-prefix`. Applied
  during collection to keep large accounts tractable.
- **Concurrency**: bounded worker pool per service, global limit configurable.
  Default conservative (8) — a scanner that trips API throttling on a production
  account will get the tool banned from the org.
- **Throttling**: adaptive retry mode from the SDK, plus explicit backoff on
  `Throttling`/`RequestLimitExceeded`.
- **Cost**: all listed calls are free. `DescribeImages` on huge ECR repos is the
  only volume risk; paginate with a cap.

Discovery on a mid-size account (~300 resources) should complete in well under a
minute. If it doesn't, the concurrency model is wrong.

## Credentials

Standard SDK chain: `--profile`, env vars, SSO, IMDS. cloud-echo does not
implement its own credential handling and never persists credentials.

For multi-account orgs, `--assume-role arn:aws:iam::...:role/CloudEchoScanner`.

## Least-privilege policy

`ReadOnlyAccess` is far broader than needed and hard to justify to a security
team. The repo ships `policies/cloud-echo-scanner.json` with exactly the actions
listed above and nothing else. This is a first-class artifact, kept in sync with
the collectors by a test that diffs the policy against the registered actions.

## Inventory cache

`.cloud-echo/inventory.json`, gitignored by default.

- Carries `scanId`, `scannedAt`, cloud-echo version, region, account, and the list
  of partial-permission warnings.
- `plan`/`graph` warn when the inventory is older than 24h but never auto-rescan.
  Silent network calls are a surprise; surprises erode trust.
- `scan --diff` compares against the previous inventory and reports drift. This is
  a natural second product surface ("what changed in prod since I built my local
  env?") but it is v2 — noted here so the data model doesn't preclude it.
