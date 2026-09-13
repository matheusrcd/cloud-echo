# 08 — Roadmap

Each milestone ends with something a real person can use. No milestone is "build
the framework"; the pipeline is built incrementally through vertical slices.

---

## M0 — Spike: validate the foundation ✅ DONE 2026-08-27

**Result: the architecture holds.** 67 PASS / 6 PARTIAL / 5 FAIL in 61 s.
`runtime: floci-ecs` stays the default, ARN fidelity is confirmed, and the Echo
Gateway is viable. Four design changes fell out of it (`RunTask` over
`CreateService`, our own API Gateway listener, a docker socket bridge, and not
trusting Floci's persistence) — none structural.

Full write-up: **[spikes/m0-findings.md](spikes/m0-findings.md)**.

➜ **Runbook: [spikes/m0-runbook.md](spikes/m0-runbook.md) · Scripts:
[`spikes/m0/`](../spikes/m0/) · Write-up: [spikes/m0-findings.md](spikes/m0-findings.md)**

```bash
cd spikes/m0 && ./run-all.sh
```

No cloud-echo code is written for this. What it answers:

- [ ] How do we detect Floci readiness? Does the 12-digit account-ID trick work?
- [ ] Do DynamoDB GSIs/streams and SQS redrive policies round-trip faithfully?
- [ ] Do RDS and ElastiCache spawn real containers, and at what hostname can a
      workload reach them?
- [ ] **`RegisterTaskDefinition` + `RunTask` + `CreateService` against Floci.**
      Does a real container start? Is `ECS_CONTAINER_METADATA_URI_V4` injected?
      Does task-role credential vending work? Does `environment[]` reach the
      container? ⚠️ *Highest-risk unknown in the whole design.*
- [ ] Lambda from a zip: real runtime or stub? Does an SQS event source mapping
      actually poll and drain?
- [ ] API Gateway v2 → Lambda: what is the invoke URL? (Floci documents none.)
- [ ] Are local ARNs byte-identical to production ARNs across every service?
- [ ] Footprint: boot time, memory for a small slice, restart persistence.

**Exit criteria:** [spikes/m0-findings.md](spikes/m0-findings.md) filled in, with
a recorded decision for D1–D5 and the affected docs amended. If Floci's ECS is
thin, [05-materializer.md](05-materializer.md) flips `runtime: docker` to the
default and we scope a metadata shim.

---

## M1 — Scan and understand · IN PROGRESS

*Deliverable: `cloud-echo scan` + `cloud-echo graph`. No local environment yet.*

- [x] The minimum collector set the linker needs: **ECS ✅ · SQS ✅ · DynamoDB ✅ ·
      Lambda ✅ · IAM ✅** (IAM as a second-phase collector over the others).
- [x] API Gateway v1 and v2, validated against a real account
      ([findings](spikes/m1-real-account-findings.md#api-gateway-round)).
- [ ] Remaining collectors: RDS, ElastiCache, plus supporting ECR, SNS, EC2,
      ELBv2, Secrets Manager, SSM.
- [x] Secret-shaped value redaction at collection time (Guarantee 2).
- [x] Validated against a real account, including a scan with only the shipped
      policy and a round trip through Floci —
      [m1-real-account-findings.md](spikes/m1-real-account-findings.md).
- [x] Read-only guard middleware + the test that proves it.
- [x] `policies/cloud-echo-scanner.json` and the drift test that keeps it honest.
- [x] Inventory model, deterministic serialization, scan runner, `scan --dry-run`.
- [x] Linker **Tier 1** with evidence, plus flow classification — validated
      against a real account ([findings](spikes/m1-real-account-findings.md#linker-round-tier-1)).
- [ ] Linker Tier 2 (config values) and Tier 3 (IAM).
- [x] `graph --format=text|json|mermaid`, `graph --explain <from> <to>`. (`dot`
      deferred: Mermaid renders on GitHub, which covers the need.)
- [x] Golden-test harness with three fixture accounts, including negative cases.

**Sequencing note.** Lambda and IAM are prioritised over the remaining primary
services because they are what the linker needs, not because they are next
alphabetically. Lambda gives Tier 1 its highest-value rule
(`lambda.event-source-mapping`, a `certain` edge needing no inference) and IAM is
the only tier that yields **direction and intent** — Tier 2 tells you a service
knows a table's name, Tier 3 tells you it writes to it. Until both exist, any
linker rule is written against imagined data.

**Foundation notes (2026-08-27).** The guard is an explicit allow-list, not the
prefix check originally sketched — a prefix check would have permitted
`GetSecretValue`, contradicting [07-security.md](07-security.md) Guarantee 2. That
allow-list is also the single source for the shipped IAM policy and
`scan --dry-run`, so the three cannot drift. Collector contract tests replay
recorded fixtures through the *real* middleware stack, which means every collector
test also exercises the guard.

**Exit criteria:** point it at a real account and have someone who knows that
account confirm the graph is right. This is the moment the core hypothesis is
validated or killed — everything after it is engineering, this part is research.

Genuinely shippable on its own: "a tool that maps and explains your AWS topology"
is useful even with zero emulation.

---

## M2 — Materialize the data plane

*Deliverable: `cloud-echo plan` + `up` for stateful resources only.*

- Blueprint schema, load/merge/validate/diff, deterministic serialization.
- Planner: seed + N-hop traversal, confidence gating, downscaling defaults.
- Floci lifecycle, network, readiness.
- Seeders: DynamoDB, SQS (+DLQ), SNS, Secrets/SSM placeholders, RDS, ElastiCache.
- Fixtures and migrations loading.
- `status`, `down`, label-based orphan reclamation.

**Exit criteria:** point an *existing* application at the local stack by hand
(`AWS_ENDPOINT_URL=...`) and have it work against real production-shaped tables
and queues. Already replaces most hand-written `docker-compose.yml` + bootstrap
scripts.

---

## M3 — Run the workloads

*Deliverable: your services actually run.*

- ECS via `floci-ecs` (or `docker`, depending on M0).
- ECR pull with digest pinning, auth refresh, platform handling.
- `image.mode: build` for local source — the "test new behaviour" path.
- Lambda deployment (zip + container), event source mappings.
- `logs`, health checks, dependency-aware failure hints.
- `cloud-echo pull` as a separate, progress-reporting step.

**Exit criteria:** `cloud-echo up` on a scanned slice produces a running service
that serves a request end-to-end against local dependencies.

---

## M4 — The edge and the gateway

*Deliverable: the differentiating feature.*

- API Gateway edge listener → workloads.
- Echo Gateway: proxy, CA generation, per-runtime trust injection, routing.
- Auto-stub for out-of-slice dependencies.
- Mock rule engine with matching and templating.
- `record` / `replay` cassettes.
- AWS-call tap: observe, mutate, block SQS/SNS sends.
- `cloud-echo mock` CLI for rule editing without a UI.

**Exit criteria:** a third-party HTTP call from a running service is intercepted,
its response edited from the CLI, and the change takes effect without a restart.

---

## M5 — Web UI

*Deliverable: `cloud-echo ui`.*

- Graph view with flow-based layout (sync spine, async branches), evidence on
  click, edge add/remove writing to the override file.
- Integrations panel: every dependency, its mode, call counts, unmocked warnings.
- Live traffic timeline over SSE; click a request to build a mock rule from it.
- Rule editor with promote-to-blueprint.

**Exit criteria:** the [00-vision.md](00-vision.md) success criteria run end to end
with no YAML editing.

---

## M6 — Iteration loop

*Deliverable: it becomes a daily driver rather than a demo.*

- `runtime: docker` with bind mounts and hot reload.
- Debugger port exposure and IDE attach docs.
- `scan --diff`: what changed in production since this blueprint was generated.
- Selective `up`/restart of a single node.

---

## Deferred (with reasons)

| Item | Why not yet |
| --- | --- |
| EventBridge rules and targets | Tier-1 linkable and cheap to add; just not on the critical path to a working v1. Likely the first post-M4 addition. |
| Step Functions | Genuinely valuable, genuinely large. Needs its own design. |
| S3 | Trivially supported by Floci, but not in the stated primary stack. Add on demand. |
| Multi-region | A fan-out change in discovery plus region-aware refs. Mostly mechanical. |
| Multi-account | Needs assume-role chains and cross-account ARN resolution. |
| Runtime observation (X-Ray, Flow Logs) | Highest-fidelity linking, but extra permissions and cost. See [03-linker.md](03-linker.md) Tier 5. |
| Real data import | Compliance minefield. See [07-security.md](07-security.md). |
| CI mode | The blueprint already makes it possible; needs ephemeral ports, headless output, and a stable exit-code contract. |
| Non-AWS clouds | Floci supports GCP/Azure/OCI. Interesting later; a distraction now. |

## Ordering rationale

M1 before M2 because the linker is the risky, novel part — build it first and find
out early if the premise holds. M2 before M3 because stateful resources are useful
standalone and workloads depend on them existing. M4 after M3 because there is
nothing to intercept until something is running. M5 last because every feature it
exposes must already work headlessly, which keeps the CLI honest.
