# 00 — Vision

## The problem

Teams running non-trivial AWS workloads have no honest local environment.

The current options all fail in the same place:

- **Emulators (Floci, LocalStack).** They emulate the *services* but know nothing
  about *your account*. You still hand-write the bootstrap: create the tables,
  create the queues, wire the event source mappings, replicate 40 env vars. That
  script drifts from production the moment someone changes a task definition, and
  nobody notices until an integration test lies to you.
- **IaC replay (Terraform against LocalStack).** Only works if 100% of your infra
  is in IaC, in one repo, with no manual drift. In practice it never is.
- **Reverse-engineering tools (Terraformer, Former2).** They produce IaC for
  *redeployment*. That is a different goal: they don't run anything locally, they
  don't model traffic, and they don't help you understand what calls what.
- **Shared dev accounts.** Slow, expensive, contended, and you cannot break them.

The missing piece is the bridge: something that reads the real account, works out
the topology, and stands that topology up locally in containers.

## What cloud-echo is

> Point cloud-echo at an AWS account and a starting resource. It discovers the
> account, infers what talks to what, and rebuilds a runnable slice of that
> topology locally on top of Floci — with every outgoing integration visible and
> mockable in real time.

Three properties define the product:

1. **Derived, not authored.** The local environment comes from the real account,
   not from a hand-maintained compose file. Re-scanning shows a diff.
2. **Faithful by default, overridable everywhere.** An ECS service runs the exact
   ECR image and the exact task definition — unless you say "use my local repo
   instead," which is one line of config.
3. **The boundary is a feature.** Everything outside the materialized slice —
   a third-party API, another team's service, an account you don't own — becomes
   an explicit, inspectable, editable mock rather than a hole.

## Why Floci

[Floci](https://floci.io/) is the emulation layer; cloud-echo never re-implements
it. Floci is MIT, has no auth tokens or feature gates, boots in ~24ms, covers the
services in our target stack, and runs Lambda/ECS/RDS/ElastiCache as *real*
containers rather than mocks. See [ADR-0002](adr/0002-floci-as-emulation-layer.md).

Division of labour:

```
cloud-echo   →  what exists, what talks to what, how to stand it up, egress control
Floci        →  AWS API surface, service semantics, container lifecycle
```

## Target stack (v1)

Primary: **ECS**, **SQS**, **Lambda**, **DynamoDB**, **RDS**, **API Gateway**,
**ElastiCache**.

Supporting (scanned because they are needed to link the primaries, not
materialized as user-facing nodes): IAM, ECR, SNS, Secrets Manager, SSM Parameter
Store, EC2 (VPC/subnet/security group), ELBv2, CloudWatch Logs.

## Primary user (v1)

A developer on their own machine who wants to run and iterate on one service and
its immediate dependencies. This biases every trade-off toward **fast iteration
over perfect reproducibility**: hot-reload matters more than byte-identical
rebuilds. CI usage is a v2 concern, but the blueprint format is designed so that
CI can consume it headlessly without a scan. See
[ADR-0004](adr/0004-cli-first-with-optional-ui.md).

## Non-goals

Stated plainly so they don't creep in:

- **Not an IaC generator.** The blueprint describes a local emulation, not a
  deployable stack. If you want Terraform, use Terraformer.
- **Not production parity.** It is a high-fidelity approximation. It will not
  catch IAM misconfiguration, real network partitions, cross-AZ latency, or
  service quotas. Docs must say this loudly; overselling it is how the project
  loses trust.
- **Not a cost, security-posture, or compliance tool.** No Cost Explorer, no
  findings, no dashboards about your bill.
- **Not a full-account cloner.** v1 materializes a *slice*, deliberately.
  See [ADR-0003](adr/0003-slice-from-seed.md).
- **Not a write path to AWS.** cloud-echo is structurally incapable of mutating
  the scanned account. See [07-security.md](07-security.md).
- **No telemetry.** Not opt-out. Not anonymous. None.

## Success criteria for v1

A developer with `ReadOnly`-equivalent access to an AWS account can, in under 15
minutes and with no prior config:

1. Run `cloud-echo scan`.
2. Run `cloud-echo plan --seed ecs/orders-api --depth 2` and see a graph they
   recognise as correct, with evidence for each edge.
3. Run `cloud-echo up` and get the service running locally against local
   DynamoDB/SQS/RDS.
4. Send a request through the local API Gateway and see it traverse the stack.
5. Open the UI, find the third-party payment call, change its response to a 500,
   and re-send — without restarting anything.

If step 2 produces a graph the developer does *not* recognise, the project has
failed at its core job. The linker is the make-or-break component.
