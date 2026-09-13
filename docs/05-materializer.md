# 05 — Materializer

Turns a blueprint into a running environment. No AWS credentials required, no
network access to AWS required (except pulling ECR images, if that mode is used).

## Boot sequence

Strictly ordered, because most of these steps have real dependencies:

```
1.  preflight        Docker reachable? disk? ports free? arch mismatch? image cache?
2.  network          create/ensure docker network `cloud-echo`
3.  socket bridge    socat container exposing the docker socket over TCP
4.  floci            start container (volume mounted), wait for health, verify a sentinel
5.  identity         create local IAM roles referenced by workloads
6.  data plane       seed in dependency order (below)
7.  gateway          start Echo Gateway, load integration rules, mint CA cert
8.  workloads        RunTask per ECS workload / register Lambdas
9.  wiring           event source mappings (after both ends exist)
10. edge             start cloud-echo's own edge listener
11. data load        fixtures / migrations (after containers are healthy)
12. report           print the port map, health, and the "what's mocked" summary
```

Steps 3, 4, 8 and 10 are shaped by what M0 measured — see
[spikes/m0-findings.md](spikes/m0-findings.md).

### The docker socket bridge (step 3)

Floci's docker-java client cannot open a mounted unix socket on Docker Desktop
for macOS; it fails with `BindException: Permission denied` against
`unix://localhost:2375`. Every container-backed service then degrades *silently* —
`RunTask` returns a task ARN and no container ever appears, and the error Floci
surfaces (`Failed to remove stale container`) points nowhere near the cause.

cloud-echo therefore always starts a `socat` bridge on the project network and
sets `DOCKER_HOST` for Floci. It is never published to the host. Cost: ~2 MiB and
one container. `doctor` detects the direct-socket failure explicitly.

### Floci storage (step 4)

`FLOCI_STORAGE_MODE=hybrid` alone does not survive a restart — the default state
path lives in the container's writable layer. A named volume must be mounted at
`/app/data`, **and it must be group-writable**, or Floci fails to boot with
`AccessDeniedException` on its write probe.

Even then, persistence proved intermittent under load in M0. Treat it as an
optimisation: after Floci reports healthy, verify a known sentinel resource and
re-seed if it is missing. Never trust the health check alone.

### Data-plane seeding order

```
DLQs  →  queues  →  topics  →  subscriptions
tables (+ streams)
secrets / SSM params
RDS instances  →  wait healthy  →  migrations
ElastiCache clusters
ECR repos (only if images are being pushed locally)
```

DLQs before queues because `RedrivePolicy` needs the target ARN. Event source
mappings are deliberately step 8, not part of seeding — a mapping created before
its consumer exists either fails or starts dead-lettering into the void.

Every seeder is **idempotent**: `up` on an already-running environment converges
rather than erroring. This is what makes the edit → `up` → test loop fast.

### What the M1 round trip adds

Rebuilding a real account's inventory inside Floci and scanning it back
([m1-real-account-findings.md](spikes/m1-real-account-findings.md)) turned up
requirements no earlier section had:

- **Queue URLs embed the endpoint.** A workload's `QUEUE_URL` points at
  production until rewritten; this is what `${ref:sqs/x.url}` is for.
- **DynamoDB stream ARNs are per-environment** (the label is a creation
  timestamp). Resolve `${ref:ddb/x.streamArn}` after the table exists; never copy.
- **Event source mappings are identified by what they connect** — source,
  function, qualifier. Their UUIDs are minted per environment.
- **Floci drops mapping qualifiers**: a mapping to `fn:live` invokes `$LATEST`
  locally. Harmless while local code is the same, wrong the day it is not.
- **Task definition revision numbers cannot be reproduced** by registering once.
- **Docker networks must be found by label and id, not by name.** Docker accepts
  duplicate names, and `docker run --network <name>` then refuses to choose — it
  broke the spike tooling after three runs.
- **Verify by exercising, not by describing.** Floci does not return `command`,
  `dependsOn` or `logConfiguration` from `DescribeTaskDefinition`, yet a real
  `RunTask` applied `command`. Describing Floci cannot confirm what was built.

## Talking to Floci

Seeding uses `aws-sdk-go-v2` with a custom endpoint resolver pointing at Floci,
and `AWS_ACCESS_KEY_ID` set to the real 12-digit account ID so local ARNs match
production ARNs exactly (see [01-architecture.md](01-architecture.md#the-account-id-trick)).

We use the SDK rather than Floci's init-script mechanism because we need
per-resource error handling, ordering, and idempotency — a pile of shell scripts
gives us none of those.

## Workload execution: two modes

This is the most consequential runtime decision, and it is per-workload.

### `runtime: floci-ecs` (default)

cloud-echo calls `RegisterTaskDefinition` then **`RunTask`** against Floci, and
Floci owns the container lifecycle.

- **Pro:** real containers, on our network, reachable by name, with task
  definition `environment[]` faithfully injected. Containers are labelled
  `floci=true` and named `floci-ecs-<taskid>-<container>`, so task → container
  mapping is exact.
- **Con:** less direct control. Bind mounts, hot-reload, and attaching a debugger
  are constrained by what Floci's ECS API exposes.

**`CreateService` does not launch tasks.** M0 confirmed it returns `ACTIVE` with
`runningCount: 0` forever and an empty `events` list — the service scheduler is
metadata only. cloud-echo still calls it so `DescribeServices` looks right to any
app that introspects it, but **`RunTask` is the execution path**, and cloud-echo
synthesises the service abstraction itself: desired count → N tasks, restart on
exit.

**Two things Floci does not provide**, both of which M0 verified absent:

| Missing | cloud-echo's answer |
| --- | --- |
| `ECS_CONTAINER_METADATA_URI_V4` | A metadata shim, deferred until someone needs it. Do not build speculatively. |
| `AWS_CONTAINER_CREDENTIALS_RELATIVE_URI` | Inject static credentials as env vars, with `AWS_ACCESS_KEY_ID` set to the 12-digit account id. |

Apps that read task metadata or assume credential rotation will not work
unmodified. Say so in the docs rather than letting users discover it.

**Floci pre-injects its own AWS env** into every ECS container —
`AWS_ENDPOINT_URL`, `AWS_ACCESS_KEY_ID=test`, `AWS_SECRET_ACCESS_KEY`,
`AWS_SESSION_TOKEN`, `FLOCI_HOSTNAME`, `FLOCI_ENDPOINT`. Task definition
`environment[]` **overrides all of them** (verified in M0), which is what makes
the Echo Gateway implementable at all: cloud-echo points `AWS_ENDPOINT_URL` at
the gateway and restores the real account id.

### `runtime: docker`

cloud-echo starts the container directly via the Docker Engine API, injecting the
same env the task definition declared.

- **Pro:** full control. Bind-mount your source, run `air`/`nodemon`, expose a
  debugger port, rebuild in two seconds.
- **Con:** no task metadata endpoint, no ECS-native credential vending. cloud-echo
  compensates by injecting static credentials and, optionally, a small shim that
  serves a task-metadata-shaped response.

**Default `floci-ecs`; switch to `docker` in the override file for the one service
you're actively editing.** That combination is the intended daily workflow: the
environment stays faithful, and the thing under your cursor stays fast.

> ✅ **Validated in M0** (2026-08-27). Real containers, working env injection, and
> correct network placement — so `floci-ecs` stays the default. Service lifecycle,
> task metadata v4, and role credential vending are all absent and are handled by
> cloud-echo as described above. Full results:
> [spikes/m0-findings.md](spikes/m0-findings.md).

## Images

Three modes per container:

| Mode | Behaviour |
| --- | --- |
| `ecr` | Pull the exact image, pinned by digest resolved at plan time. Requires ECR auth (`ecr:GetAuthorizationToken` — the one non-read-only-looking permission, and it's still read-only). |
| `build` | Build from a local context. The "test new behaviour" path. |
| `image` | Use an arbitrary local/registry image. Escape hatch. |

Practical problems to handle explicitly, because each of them will otherwise be a
first-run failure:

- **Architecture.** ECR images are usually `linux/amd64`; most developers are now
  on `arm64`. `doctor` warns; the blueprint carries a `platform` field; the docs
  are honest that emulated amd64 is slow.
- **Size.** Multi-GB images make a first `up` look broken. `cloud-echo pull` is a
  separate command with a progress bar, and `up` reports "pulling 3 images,
  ~2.1 GB" before it starts rather than hanging silently.
- **Private registries.** ECR auth tokens last 12 hours; refresh on demand rather
  than caching a stale one.
- **Images that need real infra to boot.** An app whose entrypoint waits on a real
  SSM parameter or a real service will hang. This is why every seeded resource
  exists *before* workloads start, and why unmatched egress hits a mock instead of
  hanging: a hard 501 from the gateway is a far better failure than a 60-second
  connect timeout.

## Downscaling

Production shapes are wrong for a laptop. Applied automatically, always reported:

| Production | Local |
| --- | --- |
| `desiredCount: 4` | `1` |
| `db.r6g.4xlarge` | `postgres:16-alpine`, default resources |
| Multi-AZ RDS | single instance |
| ElastiCache 3-node replication group | single Valkey |
| Provisioned DynamoDB 500 WCU | on-demand |
| Lambda reserved concurrency 200 | unset |

Overridable, but the defaults must assume a 16 GB laptop. `up` prints a resource
estimate and refuses to start if the requested footprint exceeds available memory
— failing fast beats swapping to death.

## Teardown and state

- `down` stops containers, keeps volumes.
- `down --volumes` drops all state, including RDS data and Floci persistence.
- Floci's `storage: hybrid` is the default so DynamoDB/SQS state survives a
  restart. `memory` for anyone who wants a clean slate every time.
- Everything cloud-echo creates is labelled `cloud-echo.project=<name>` so orphans
  from a crashed run are reliably reclaimable — `cloud-echo down --force` finds
  them by label, not by a stale pidfile.

## Failure reporting

The most common support question will be "why isn't my service up?" `status` must
answer it without the user reading raw logs:

```
FLOCI          healthy    :4566
GATEWAY        healthy    :7070   12 integrations, 3 mocked
rds/orders-db  healthy    :5432   migrations: 14 applied
ecs/orders-api UNHEALTHY  :8080   restarting (3) — last: connect ECONNREFUSED
                                  ↳ hint: DB_HOST resolves to rds/orders-db,
                                    which became healthy 4s after this container
                                    started. Retry policy missing?
```

Correlating a workload failure back to the blueprint node and its dependencies is
a small amount of work with a large payoff.
