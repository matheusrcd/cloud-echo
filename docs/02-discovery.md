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
    Raw       json.RawMessage   // untouched API response (for evidence + future rules)
    Source    Provenance        // {api: "ecs:DescribeServices", collectedAt: ...}
}
```

Keeping `Raw` matters: new linker heuristics can be developed and tested against
old inventories without re-scanning.

### Resource IDs

IDs are `<service>/<scope...>/<name>`, scoped by whatever AWS actually uses to
make the name unique:

| | |
| --- | --- |
| `ecs/cluster/main` | cluster |
| `ecs/main/orders-api` | service — **scoped by cluster** |
| `ecs/taskdef/orders-api:41` | task definition, family + revision |

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
| **Lambda** | `ListFunctions`, `GetFunctionConfiguration`, `ListEventSourceMappings`, `GetPolicy`, `ListFunctionUrlConfigs`, `GetFunctionCodeSigningConfig` | `Environment.Variables`, `Role`, `ImageUri`, `Handler`, `Runtime`, event source ARNs, resource-policy principals |
| **SQS** | `ListQueues`, `GetQueueAttributes`, `ListQueueTags` | `RedrivePolicy` (→ DLQ), `VisibilityTimeout`, `Policy` (→ who can send), `FifoQueue` |
| **DynamoDB** | `ListTables`, `DescribeTable`, `DescribeTimeToLive`, `DescribeContinuousBackups` | key schema, GSIs/LSIs, `StreamSpecification` |
| **RDS** | `DescribeDBInstances`, `DescribeDBClusters`, `DescribeDBSubnetGroups` | `Engine`, `EngineVersion`, `Endpoint`, `Port`, `DBName`, `VpcSecurityGroups` |
| **API Gateway v1** | `GetRestApis`, `GetResources`, `GetMethod`, `GetIntegration`, `GetStages`, `GetAuthorizers` | integration `uri`, `type`, `connectionId` |
| **API Gateway v2** | `GetApis`, `GetRoutes`, `GetIntegrations`, `GetStages`, `GetAuthorizers` | same |
| **ElastiCache** | `DescribeReplicationGroups` (Redis/Valkey), `DescribeCacheClusters` (memcached only) | `Engine`, `EngineVersion`, endpoint, port |

> **Tags come from `Include`, not a second call.** Every ECS `Describe*` accepts
> an `Include: [TAGS]` parameter, so `ListTagsForResource` is not needed and is
> not on the allow-list. Worth checking per service before adding a tag call.

> **`ListTasks`/`DescribeTasks` are not collected in v1.** The task *definition*
> attached to a service is what the local environment reproduces; the running
> tasks add nothing the materializer uses. They were on the original list, and
> asking for a permission we never exercise is exactly what the drift test exists
> to prevent.

> **ElastiCache is two APIs, not one.** Redis and Valkey clusters are *only*
> visible through `DescribeReplicationGroups`; `DescribeCacheClusters` covers
> memcached. M0 confirmed Floci enforces the same split as modern AWS
> (`CreateCacheCluster` rejects Redis/Valkey with *"Engine must be 'memcached'"*).
> A collector that reads only `DescribeCacheClusters` will silently miss every
> Redis cluster in the account.

### Supporting services

| Service | Calls | Why |
| --- | --- | --- |
| **IAM** | `GetRole`, `ListAttachedRolePolicies`, `GetPolicy`, `GetPolicyVersion`, `ListRolePolicies`, `GetRolePolicy` | Tier-3 permission-based edge inference — the highest-value heuristic |
| **ECR** | `DescribeRepositories`, `DescribeImages` | resolve image tag → digest so the local env is pinned |
| **SNS** | `ListTopics`, `GetTopicAttributes`, `ListSubscriptionsByTopic` | fan-out edges |
| **Secrets Manager** | `ListSecrets`, `DescribeSecret` | **never `GetSecretValue`** by default |
| **SSM** | `DescribeParameters`, `GetParameters` (String/StringList only) | config values are a rich linking signal; SecureString is skipped |
| **EC2** | `DescribeVpcs`, `DescribeSubnets`, `DescribeSecurityGroups` | Tier-4 reachability corroboration |
| **ELBv2** | `DescribeLoadBalancers`, `DescribeTargetGroups`, `DescribeListeners`, `DescribeRules` | API GW / ALB → ECS path |
| **CloudWatch Logs** | `DescribeLogGroups` | map workloads to log groups (used later for runtime observation) |

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
