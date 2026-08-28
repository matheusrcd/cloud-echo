# M0 Spike — Findings

**Run on:** 2026-08-27
**Host:** macOS (Darwin 25.5.0), arm64, Docker Desktop 4.21.0 / Engine 24.0.2, 8 GB RAM (4 GB to Docker)
**Floci:** `floci/floci:latest` → `FLOCI_VERSION=1.7.0`, 332 MB image
**Result:** 67 PASS · 6 PARTIAL · 5 FAIL, full suite in **61 s**

**Verdict: the architecture holds.** `runtime: floci-ecs` stays the default, the
blueprint reference model works, and the Echo Gateway is viable. Four things need
design changes; none of them are structural.

---

## The headline

Floci runs real containers for ECS, RDS, ElastiCache, Lambda, and even ECR — but
**only after working around a Docker socket bug**, and it is thinner than
advertised in three specific places.

### ⚠️ Blocker found and worked around: the Docker socket

The first ECS and RDS runs both failed with:

```
java.net.BindException: Permission denied
  ... when sending request to unix://localhost:2375
```

Floci's docker-java client (GraalVM native-image) **cannot open the mounted unix
socket** on Docker Desktop for macOS, even though the socket is present and
`curl --unix-socket` works fine from inside the same container. Without this,
every container-backed service silently degrades: `RunTask` returns a task ARN
and no container ever appears.

The error message is actively misleading — RDS reports
`Failed to remove stale container floci-rds-db-...`, which reads like a cleanup
problem, not a connectivity one.

**Workaround:** bridge the socket over TCP with `socat` on the project network and
point Floci at it with `DOCKER_HOST`. Not published to the host.

```bash
docker run -d --name ce-dockerproxy --network ce-m0 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  alpine/socat TCP-LISTEN:2375,fork,reuseaddr UNIX-CONNECT:/var/run/docker.sock

docker run -d --name ce-floci --network ce-m0 -p 4566:4566 \
  -e DOCKER_HOST=tcp://ce-dockerproxy:2375 ...
```

With the bridge in place, everything container-backed started working
immediately. See [D6](#d6--docker-socket-bridge).

---

## Results

| Probe | Status | Note |
| --- | --- | --- |
| `floci.ready` | PASS | first API response **2.2 s** after `docker run` (incl. image start) |
| `floci.health` | PASS | `/_localstack/health` → 200. LocalStack-compatible |
| `floci.account-id` | PASS | 12-digit access key adopted as the account id |
| `ddb.create-table` / `gsi` / `stream` | PASS | composite key + GSI + stream all round-trip |
| `ddb.gsi-query` | PASS | GSI is queryable, not just declared |
| `sqs.redrive-readback` | PASS | `RedrivePolicy` survives `GetQueueAttributes` |
| `secrets.roundtrip` | PASS | placeholder secrets work |
| `rds.create` / `available` | PASS | Postgres **16.3**, real `floci-rds-db-*` container |
| `rds.network` | PASS | lands on our docker network |
| `rds.connect` | PASS | real `psql` connection, `select version()` works |
| `cache.create` | PASS | **only via `CreateReplicationGroup`** (see below) |
| `cache.endpoint` | **FAIL** | `NodeGroups: null` — no endpoint discoverable via the API |
| **`ecs.real-container`** | **PASS** | real container `floci-ecs-<taskid>-<name>`, labelled `floci=true` |
| **`ecs.env-injection`** | **PASS** | task definition `environment[]` reaches the container |
| **`ecs.metadata-v4`** | **FAIL** | `ECS_CONTAINER_METADATA_URI_V4` not injected |
| **`ecs.task-role-creds`** | **FAIL** | `AWS_CONTAINER_CREDENTIALS_RELATIVE_URI` not injected |
| `ecs.container-network` | PASS | on our network, reaches Floci and siblings by name |
| `ecs.service-running` | **PARTIAL** | `CreateService` never launches tasks — see [D2](#d2--use-runtask-not-createservice) |
| `ecs.port-mapping` | PARTIAL | no published host ports |
| `lambda.create` / `active` / `invoke` | PASS | real container `floci-ce-fn-*`, honours `memory-size` |
| `lambda.esm-delivery` | PASS | SQS mapping drains the queue in **~6 s** |
| `lambda.logs` | PARTIAL | log group exists; `get-log-events` returned nothing |
| `apigw.create-*` | PASS | api, integration, route, stage all creatable |
| `apigw.invoke` | **FAIL** | **no reachable invoke URL** — see [D5](#d5--api-gateway-needs-our-own-listener) |
| `arn.*` (6 services) | PASS | **all byte-identical to production ARNs** |
| `account.isolation` | PASS | a different account id sees 0 resources |
| `footprint.memory` | PASS | **147 MiB total for 7 containers** |
| `footprint.restart` | PASS | back up in 1–2 s |
| `footprint.persistence` | **INTERMITTENT** | see [D7](#d7--do-not-rely-on-floci-persistence) |

Three of the original `PARTIAL`s were **bugs in the probes, not in Floci**, and
have been fixed: the ElastiCache engine call, the Lambda payload matcher, and the
network-membership check.

---

## Decisions

### D1 — `runtime: floci-ecs` stays the default

**Evidence:** `ecs.real-container` PASS, `ecs.env-injection` PASS,
`ecs.metadata-v4` FAIL, `ecs.task-role-creds` FAIL.

Per the runbook's decision rule this is the middle case: real containers and
working env injection, but no ECS-native metadata or credential vending.

**Decision:** keep `floci-ecs` as the default in
[04-blueprint.md](../04-blueprint.md). Add to M3:

- inject static credentials as env vars (`AWS_ACCESS_KEY_ID` = the 12-digit
  account id) instead of relying on `AWS_CONTAINER_CREDENTIALS_RELATIVE_URI`
- a **task metadata shim** for apps that read `ECS_CONTAINER_METADATA_URI_V4`,
  deferred until someone actually needs it — do not build it speculatively

**Docs to amend:** [05-materializer.md](../05-materializer.md) currently claims
the metadata endpoint and role vending "behave like the real thing." That is
false and must be corrected.

### D2 — Use `RunTask`, not `CreateService`

**Evidence:** `CreateService` returns success and `status: ACTIVE`, but
`runningCount` stays `0` forever, `deployments[].runningCount` is `0`, and
`events` is empty. Both running containers came from `RunTask`. Consistent with
Floci's documented limitation that `pendingCount` is always zero — the service
scheduler appears to be metadata only.

**Decision:** the materializer launches ECS workloads with `RunTask`, and
synthesises the service abstraction itself (desired count → N tasks, restart on
exit). `CreateService` is still called so `DescribeServices` looks right to any
app that introspects it, but it is not the execution path.

**Consequence:** `desiredCount` downscaling in
[05-materializer.md](../05-materializer.md) becomes cloud-echo's job, not Floci's.

### D3 — ARN fidelity confirmed; keep the claim

**Evidence:** all six services return byte-identical production ARNs, and the SQS
queue URL contains the real account id. Account isolation works.

**Decision:** [01-architecture.md](../01-architecture.md) keeps the account-id
trick as written. This is the cheapest high-value behaviour found in the spike.

**Caveat:** Floci injects `AWS_ACCESS_KEY_ID=test` into ECS containers by default,
which would break ARN fidelity *for resources the workload itself creates*. The
task definition must override it — which it can (verified).

### D4 — `${ref:}` targets for RDS and ElastiCache

**Evidence:** the RDS endpoint reported by `DescribeDBInstances` was
`172.20.0.2:7001` — the **Floci container's IP**, not the Postgres container's
(`172.20.0.6`). Floci proxies the connection. Confirmed reachable as
`ce-m0-floci:7001` by container name, and Valkey answered `PONG` on
`ce-m0-floci:6379`.

**Decision:** `${ref:rds/x.host}` resolves to the **Floci container name**, not
the reported IP and not the backing container. Raw IPs change between runs; the
name is stable.

For ElastiCache, `NodeGroups` is `null`, so there is no discoverable port.
cloud-echo synthesises the endpoint from the Floci host plus the conventional
port, and records that as a known gap.

**Also:** `CreateCacheCluster` rejects Redis/Valkey outright — *"Engine must be
'memcached'. For Redis/Valkey use CreateReplicationGroup."* This matches modern
AWS. The discovery collector in [02-discovery.md](../02-discovery.md) must read
`DescribeReplicationGroups` for Redis/Valkey and `DescribeCacheClusters` only for
memcached. **The doc currently implies both are interchangeable.**

### D5 — API Gateway needs our own listener

**Evidence:** ten candidate URL shapes tried, none returned 200. Floci reports
`ApiEndpoint: https://<id>.execute-api.us-east-1.amazonaws.com`, which is the real
AWS URL and does not resolve locally. The `400` responses turned out to come from
the **S3 handler** (`POST on bucket requires ?delete parameter`) — virtual-hosted-
style bucket detection swallowing the request. No handler logged the request at
all, so there is no reachable API Gateway data plane.

**Decision:** cloud-echo runs its **own edge listener** on a clean host port and
routes from the blueprint's `edge.routes`, rather than proxying to Floci's API
Gateway. This was already the fallback in [05-materializer.md](../05-materializer.md)
step 9; it is now the primary design.

This is arguably better anyway: the Echo Gateway is already in the path, the port
is predictable, and we control routing and observability end to end.

### D6 — Docker socket bridge

**Decision:** cloud-echo starts a socat bridge container and sets `DOCKER_HOST`
for Floci, on macOS **and** Linux. Cost is ~2 MiB and one container; the failure
mode it prevents is silent and very hard to diagnose.

`doctor` must detect the failing direct-socket case explicitly, because the error
Floci produces (`Failed to remove stale container`) points nowhere near the real
cause.

**Upstream:** worth reporting to Floci ([Q9](../09-open-questions.md#q9--relationship-with-floci-upstream)).
Reproduction is small and the diagnostic message is fixable.

### D7 — Do not rely on Floci persistence

**Evidence:** mixed, and deliberately not forced into a clean answer.

- Persistence requires a **mounted volume** — `FLOCI_STORAGE_MODE=hybrid` alone
  is not enough, since the default path is inside the container's writable layer.
- The volume must be group-writable or Floci **fails to boot** with
  `AccessDeniedException: /data/.floci-write-probe*.tmp`.
- With the volume mounted at `/app/data`, isolated tests pass consistently:
  simple tables, tables with GSIs, tables with streams, SQS queues, immediate
  restart, and `stop`/`start` all survive.
- **But in the full suite run it failed**, and one restart logged
  `Loaded 0 entries from /app/data/dynamodb-tables.json` against a file
  containing 2449 bytes of valid table JSON.

The leading hypothesis is a race between the readiness signal and the storage
load — `sts:GetCallerIdentity` answers before state finishes loading — but this
was **not isolated**, and it may be something else.

**Decision:** treat persistence as an optimisation, never a dependency. This is
already the right design: seeders are idempotent and `up` converges, so a lost
state directory costs a few seconds of re-seeding, not correctness. cloud-echo
should verify a known sentinel resource after Floci reports ready, and re-seed
if it is missing, rather than trusting the health check.

**Follow-up:** worth isolating properly before M2 ships, because it also affects
how much we can trust the readiness signal in general.

---

## Surprises

1. **Floci pre-injects AWS env into ECS containers** — `AWS_ENDPOINT_URL`,
   `AWS_ACCESS_KEY_ID=test`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`,
   `FLOCI_HOSTNAME=localhost.floci.io`, `FLOCI_ENDPOINT`. **Verified that task
   definition `environment[]` overrides all of them** — which is what makes
   [ADR-0005](../adr/0005-single-egress-chokepoint.md) implementable. Had this not
   been overridable, the Echo Gateway would have been dead on arrival.

2. **`localhost.floci.io` resolves correctly from inside containers** and reaches
   Floci. Floci is doing DNS/hosts work we do not have to replicate.

3. **Floci labels its children `floci=true`** and names them predictably
   (`floci-ecs-<taskid>-<container>`, `floci-rds-db-<id>`, `floci-valkey-<id>`,
   `floci-ce-fn-<hash>`). This makes task → container mapping trivial and
   validates the labelling approach in [05-materializer.md](../05-materializer.md).

4. **Floci runs a real ECR registry container** (`floci-ecr-registry`), so
   `image.mode: ecr` has a local target to push to.

5. **Lambda honours `memory-size`** as a real container memory limit (512 MiB
   observed), so resource estimation can be accurate.

6. **The footprint is far better than expected**: 147 MiB total across 7
   containers, ~1% of an 8 GB host. The `up` memory-guard in
   [05-materializer.md](../05-materializer.md) is less urgent than assumed —
   real ECR images, not Floci, will be the constraint.

7. **Full suite runs in 61 s**, which makes this a viable regression test against
   future Floci releases rather than a one-off.

---

## Impact on the roadmap

- [x] **M0 complete.** No structural change to the architecture.
- [ ] Amend [05-materializer.md](../05-materializer.md): remove the metadata/role
      vending claim; `RunTask` not `CreateService`; own edge listener; socat
      bridge; volume + ownership requirement.
- [ ] Amend [02-discovery.md](../02-discovery.md): ElastiCache Redis/Valkey via
      `DescribeReplicationGroups`.
- [ ] Amend [04-blueprint.md](../04-blueprint.md): `${ref:rds/*.host}` resolves to
      the Floci container name; document the ElastiCache endpoint gap.
- [ ] Close [Q1](../09-open-questions.md#q1--how-complete-is-flocis-ecs) — answered.
- [ ] New question: isolate the persistence/readiness race (D7).
- [ ] New question: report the Docker socket bug upstream (D6).
- [ ] **M1 scope unchanged.** Discovery and the linker are untouched by all of
      this — they do not depend on any of it.

## Fixtures harvested

`spikes/m0/.work/*.json` holds real Floci API responses. Worth promoting to
`testdata/` when the collectors are written:

- `ddb-describe.json` — table with GSI + stream, the shape the DynamoDB collector
  must parse
- `sqs-attributes.json` — includes a real `RedrivePolicy` string for the Tier-1
  DLQ linker rule
- `rds-describe.json` — engine/version/endpoint shape
- `ecs-taskdef.json`, `ecs-container-env.txt` — the container env Floci injects,
  needed to write the override logic correctly
- `apigw-api.json` — v2 API shape

These are Floci's responses, not AWS's. Useful for materializer tests; **not** a
substitute for real AWS fixtures in the discovery collectors' contract tests.
