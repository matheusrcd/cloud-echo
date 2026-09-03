# M0 Spike — Runbook

**Goal:** answer, empirically, the questions the architecture currently assumes.
No cloud-echo code is written until this is done. Finding out in M3 that Floci's
ECS is a stub would invalidate weeks of work.

**This is an exploration, not a test suite.** A `FAIL` is a finding, not a
problem to fix. The deliverable is
[`m0-findings.md`](m0-findings.md), not a green run.

## Running it

```bash
cd spikes/m0
./run-all.sh              # everything, ~5-10 min (first run pulls images)
./run-all.sh 04           # just the ECS probe (Floci must already be up)
./99-teardown.sh          # clean up, keep the findings
./99-teardown.sh --all    # clean up, delete the findings too
```

Requirements: Docker running, AWS CLI v2, `jq`, `zip`, `python3`, `curl`.
No AWS credentials — nothing here touches a real account.

Raw API responses land in `spikes/m0/.work/` (gitignored). Keep them: they are
the evidence behind the write-up, and they double as the first fixtures for the
discovery collectors' contract tests.

## What each probe decides

| Probe | Question | What a bad answer changes |
| --- | --- | --- |
| **00** preflight | Docker up? arch? ports? socket? | Nothing — gates the rest |
| **01** floci-up | How do we detect readiness? Does the 12-digit account trick work? | No health endpoint → poll `sts:GetCallerIdentity`. No account trick → drop the ARN-fidelity claim from [01-architecture.md](../01-architecture.md) |
| **02** data plane | Do GSIs, streams and redrive policies round-trip? | Missing streams → Lambda stream triggers unsupported. Redrive not read back → the linker's Tier-1 DLQ rule has no local equivalent |
| **03** rds + cache | Do real DB containers appear, and at what hostname? | Decides what `${ref:rds/x.host}` resolves to in [04-blueprint.md](../04-blueprint.md). If containers land off our network, the materializer must attach them |
| **04** **ECS** ⚠ | Real containers? Metadata v4? Task-role creds? Env injection? | **The big one.** See below |
| **05** lambda + esm | Real runtime, or a stub? Does an SQS mapping actually poll? | ESM not polling → async flows can't be tested locally, which guts a large part of the value |
| **06** api gateway | What is the invoke URL? | No usable URL → cloud-echo fronts API GW with its own listener on a clean port (probably better anyway) |
| **07** arn fidelity | Are local ARNs byte-identical to production? | Not identical → the blueprint must rewrite every ARN-bearing env var, and apps with hardcoded ARNs break |
| **08** footprint | What does a small slice actually cost? | Feeds the `up` pre-flight estimate. Also validates that `storage: hybrid` survives a restart |

## Probe 04 in detail — the decision that matters

[Q1 in the open questions](../09-open-questions.md#q1--how-complete-is-flocis-ecs).

Floci's own docs confirm 58 ECS operations and that "in the default configuration
tasks run as real Docker containers." They say **nothing** about the ECS task
metadata endpoint or task-role credential vending. Those two are what separate
"faithful ECS emulation" from "a container launcher with an ECS-shaped API."

Four sub-probes, and how each outcome changes the design:

| Sub-probe | If it passes | If it fails |
| --- | --- | --- |
| `ecs.real-container` | `runtime: floci-ecs` is viable | ECS is mock-mode only → **`runtime: docker` becomes the only option**; rewrite [05-materializer.md](../05-materializer.md) |
| `ecs.env-injection` | `${ref:}` substitution works through task definitions | Blocks the entire blueprint reference model — this must work in one runtime or the other |
| `ecs.metadata-v4` | Apps reading task metadata work unmodified | We write a metadata shim sidecar; scope for M3 grows |
| `ecs.task-role-creds` | Apps get credentials the ECS-native way | Inject static credentials as env vars instead — works for most apps, breaks any that assume credential rotation |

**The decision rule:**

- `real-container` + `env-injection` pass → keep `floci-ecs` as the default.
- `real-container` passes, metadata/creds fail → keep `floci-ecs` as the default,
  add a metadata shim to M3, and document the gap honestly.
- `real-container` fails → `runtime: docker` becomes the default, `floci-ecs`
  becomes an opt-in, and [ADR-0002](../adr/0002-floci-as-emulation-layer.md) gets
  a consequences amendment. Non-trivial scope change; better to know now.

Whatever happens, read `spikes/m0/.work/ecs-probe-logs.txt` by hand. The probe
container prints its full environment; the automated checks look for specific
markers but the raw log may show something the checks did not anticipate.

## After the run

1. Fill in [`m0-findings.md`](m0-findings.md) — one row per probe, plus the
   decisions section. Be specific: "PARTIAL" with no detail is worthless in three
   weeks.
2. Amend the affected docs. If probe 03 shows RDS containers land on a different
   network, [05-materializer.md](../05-materializer.md) gets a step. If probe 07
   shows ARNs differ, remove the claim from
   [01-architecture.md](../01-architecture.md) — do not leave an aspirational
   statement in a design doc.
3. Close or update [Q1, Q6, Q7](../09-open-questions.md), and open new questions
   for anything surprising.
4. Only then start M1.

## A note on what this spike does *not* cover

- **Real ECR images.** Every probe uses `alpine`. Pulling a multi-GB production
  image with `linux/amd64` on an arm64 host is a real problem
  ([05-materializer.md](../05-materializer.md#images)) but it is a known
  engineering cost, not an unknown. M3 handles it.
- **The Echo Gateway.** Nothing to intercept until workloads run. M4.
- **Discovery.** Runs against real AWS, is well-understood, and is independently
  testable. M1.
