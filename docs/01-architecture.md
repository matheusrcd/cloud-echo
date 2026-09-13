# 01 — Architecture

## Pipeline

Five stages, strictly one-directional. Each stage has a persisted artifact, so any
stage can be re-run or replaced without re-running the ones before it.

```
   AWS account (read-only)
          │
          ▼
   ┌─────────────┐
   │  DISCOVERY  │  parallel per-service collectors
   └─────────────┘
          │  .cloud-echo/inventory.json      (raw, gitignored)
          ▼
   ┌─────────────┐
   │   LINKER    │  edge inference + flow classification
   └─────────────┘
          │  .cloud-echo/graph.json          (derived, gitignored)
          ▼
   ┌─────────────┐
   │   PLANNER   │  seed selection + N-hop traversal + defaults
   └─────────────┘
          │  cloud-echo.yaml                 (THE BLUEPRINT — committed)
          │  cloud-echo.override.yaml        (user edits — committed)
          ▼
   ┌─────────────┐
   │MATERIALIZER │  Floci seeding + container orchestration
   └─────────────┘
          │
          ▼
   ┌──────────────────────────────────────────────┐
   │  RUNTIME:  Floci + workloads + Echo Gateway   │
   └──────────────────────────────────────────────┘
```

The critical property: **only DISCOVERY touches AWS.** Everything downstream works
offline from `inventory.json`. This means iterating on the linker or replanning a
slice costs nothing, and it makes the whole pipeline testable against recorded
fixtures.

## Components

### `cloud-echo` CLI (Go binary)

Single static binary, no runtime dependency. Commands:

| Command | Does |
| --- | --- |
| `scan` | Discovery. Writes `inventory.json`. Only command that talks to AWS. |
| `graph` | Runs the linker, prints/exports the graph. Offline. |
| `plan` | Seed + depth → generates `cloud-echo.yaml`. Offline. Shows a diff on re-run. |
| `up` | Materializes and starts everything. |
| `down` | Tears down. `--volumes` to drop state. |
| `status` | What's running, health, port map. |
| `logs <node>` | Tails a workload. |
| `ui` | Serves the web UI (graph + integrations + live mock editing). |
| `mock <integration>` | Read/write mock rules from the CLI, no UI needed. |
| `doctor` | Preflight: Docker reachable, Floci pullable, disk, arch mismatch, creds. |

### Echo Gateway (container)

The single egress choke point for every workload container. Routes each outbound
call to Floci, to a local sibling container, or to a mock, and records everything.
Details in [06-echo-gateway.md](06-echo-gateway.md).

### Control plane (embedded HTTP server)

Runs inside the CLI process during `up`/`ui`. Serves the React UI (embedded via
`embed.FS`, no separate install), the graph, live traffic from the gateway over
SSE, and the mock-rule write API.

### Floci

Managed as a container by the materializer, not via `floci-cli` — we need
programmatic lifecycle control and a known network. We do reuse Floci's health
endpoint for readiness.

## Runtime topology

```
                        ┌──────────────────────────────────┐
                        │  docker network: cloud-echo       │
                        │                                   │
  host :8080 ───────────┼──► apigw-edge ──┐                 │
                        │                 │                 │
  host :4566 ───────────┼──► floci ◄──────┤                 │
                        │      │          │                 │
                        │      │  (real containers spawned  │
                        │      │   by Floci: rds, valkey,   │
                        │      │   lambda runtimes)         │
                        │      ▼                            │
                        │   postgres  valkey  lambda-*      │
                        │                 ▲                 │
                        │                 │                 │
  host :7070 (ctl) ◄────┼── echo-gateway ─┘                 │
                        │      ▲                            │
                        │      │  HTTP_PROXY + AWS_ENDPOINT  │
                        │      │                            │
                        │   ┌──┴────────────────┐           │
                        │   │ workload containers│          │
                        │   │ (ECS services)     │          │
                        │   └────────────────────┘          │
                        └──────────────────────────────────┘
                                     │
                                     ▼
                        mocked / recorded third parties
```

Every workload container gets:

- `AWS_ENDPOINT_URL` pointing at the gateway (which forwards to Floci)
- `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` pointing at the gateway
- the gateway's generated CA cert, plus the per-runtime env var that makes the
  language trust it
- resolved refs (queue URLs, table names, DB hostnames) substituted into the env
  vars the real task definition declared

**Everything a workload sends leaves through the gateway.** That single decision
is what makes "see and edit every integration" a uniform feature instead of seven
special cases. See [ADR-0005](adr/0005-single-egress-chokepoint.md).

## The account-ID trick

Floci uses `AWS_ACCESS_KEY_ID` as the account ID when it is exactly 12 digits.
cloud-echo exploits this: it injects the **real account ID** as the local access
key, so locally-created ARNs are byte-identical to production ARNs.

Consequence: an application with a hardcoded
`arn:aws:sqs:us-east-1:123456789012:orders` in a config file works locally with no
change. This is worth a lot and costs nothing. It is enabled by default and
disabled by `--anonymize-account`.

## Repository layout (proposed)

```
cmd/cloud-echo/          CLI entrypoint
internal/
  discovery/             one file per AWS service collector
    collector.go         Collector interface
    ecs.go sqs.go ...
  inventory/             normalized resource model + serialization
    spec/                the spec types and id scheme — the contract every stage reads
  linker/
    rule_*.go            one file per inference rule (tiered)
    linker.go            context, node set, flow classification
  blueprint/             schema, load, merge, validate, diff
  planner/               seed traversal, defaults, downscaling
  materializer/
    floci/               Floci lifecycle + readiness
    seed/                one seeder per resource type
    workload/            container orchestration
  gateway/               proxy, router, mock engine, recorder
  ui/                    embedded React app + control-plane handlers
docs/
testdata/
  accounts/              recorded AWS API fixtures for linker tests
```

## Language and dependency choices

- **Go 1.24+.** Single binary, mature `aws-sdk-go-v2`, first-class Docker control,
  cheap concurrency for scanning. See [ADR-0001](adr/0001-go-for-the-core.md).
  (ADR-0001 says 1.23; current `aws-sdk-go-v2` declares `go 1.24`, so 1.24 is the
  real floor. Not a decision, just what the dependency requires.)
- **Docker Engine API** via `github.com/docker/docker/client`, not shelling out to
  the `docker` CLI. We need events, logs, and health streams.
- **No Kubernetes, no Terraform, no Pulumi** anywhere in the dependency tree.
- UI: React + Vite, built into `internal/ui/dist`, embedded. `go build` alone must
  produce a working binary from a checked-in build artifact, so contributors who
  only touch Go never need Node.

## Testing strategy

The pipeline shape makes this tractable:

- **Discovery**: contract tests against recorded HTTP fixtures. No live AWS in CI.
- **Linker**: golden tests. `internal/linker/testdata/<account>/inventory.json` →
  `expected-graph.json`. Every new heuristic ships with a fixture, including
  negative cases (things it must *not* link).
- **Blueprint**: round-trip and merge tests; schema validation.
- **Materializer**: integration tests against a real Floci container in CI. Slow
  tier, runs on PR to `main` only.
- **Gateway**: unit tests for routing/matching, plus one end-to-end test that
  proves a workload's egress actually lands on a mock.
