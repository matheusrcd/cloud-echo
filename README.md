# cloud-echo

[![CI](https://github.com/matheusrcd/cloud-echo/actions/workflows/ci.yml/badge.svg)](https://github.com/matheusrcd/cloud-echo/actions/workflows/ci.yml)

Scans your AWS account and rebuilds it as a containerized local environment using
[Floci](https://floci.io/).

> **Status: early. Not usable yet.** The architecture is designed and validated
> ([M0 spike](docs/spikes/m0-findings.md)), and discovery is built and validated
> against a real account — read-only guard, inventory model, collectors for seven
> services — and the linker reads its first three tiers. Everything
> below marked ⬚ does not exist. Feedback on the design is still the most useful
> contribution.

## The idea

Point cloud-echo at an AWS account and a starting resource. It discovers the
account, infers what talks to what, and rebuilds a runnable slice of that topology
locally — with every outgoing integration visible and mockable in real time.

```bash
cloud-echo scan                                    # ◐ read-only discovery (7 services so far)
cloud-echo graph --explain ecs/orders-api          # ◐ what does it talk to, and why? (Tiers 1–3)
cloud-echo plan --seed ecs/orders-api --depth 2    # ⬚ → cloud-echo.yaml
cloud-echo up                                      # ⬚ Floci + your containers, running
cloud-echo ui                                      # ⬚ graph + live traffic + edit mocks
```

What works today: `cloud-echo scan --dry-run` prints the exact API surface a scan
would touch, and `cloud-echo scan` reads ECS, SQS, DynamoDB, Lambda, IAM, API
Gateway and RDS into a normalized inventory, validated against a real account.
`cloud-echo graph` links it — declared relationships (event source mappings,
redrive, API integrations, authorizers), what configuration names (queue URLs,
ARNs, table names, third-party URLs in env vars, command lines and stage
variables) and what each workload's IAM role lets it do with them (publish,
consume, read, write) — classifies every node as entrypoint, sync, async or
unreached, and explains any edge with `--explain <from> <to>`. `--format mermaid`
draws it.

Your ECS service runs the exact image from ECR with the exact task definition.
Its DynamoDB tables, SQS queues, Postgres, and Valkey are real and local. The
third-party payment API it calls is a mock whose response you can change from a
browser without restarting anything.

## Why

Emulators like Floci emulate the *services* but know nothing about *your account* —
you still hand-write the bootstrap, and it drifts from production silently. IaC
replay only works if all your infra is in IaC with no manual drift. Tools like
Terraformer produce IaC for redeployment, not something you can run.

cloud-echo is the bridge: read the real account, work out the topology, stand it
up locally.

## What it is not

- Not an IaC generator — the blueprint describes a local emulation, not a
  deployable stack.
- Not production parity — it's a high-fidelity approximation, not a staging
  replacement.
- Not a write path to AWS — it is [structurally incapable](docs/adr/0006-read-only-by-construction.md)
  of calling a mutating AWS API.
- No telemetry. Not opt-out — absent.

## What it does to your AWS account

Nothing. An SDK middleware rejects every operation not on an explicit allow-list
of exact read operations, before the request is serialized, with
[no flag to disable it](docs/adr/0007-allow-list-over-name-prefix.md).

```bash
cloud-echo scan --dry-run    # every API call it would make, without credentials
```

[`policies/cloud-echo-scanner.json`](policies/cloud-echo-scanner.json) grants
exactly those actions and is generated from the same list, with a test that fails
on drift. `secretsmanager:GetSecretValue` is deliberately absent — secret values
never leave AWS — and API Gateway's coarse `apigateway:GET` is scoped to API
definitions, so the role cannot read API key values
([ADR-0008](docs/adr/0008-scope-coarse-iam-actions.md)).

## Building

Requires Go 1.24+.

```bash
go build ./cmd/cloud-echo
go test ./...
```

## Design docs

Start with [docs/00-vision.md](docs/00-vision.md), then
[docs/01-architecture.md](docs/01-architecture.md).
Decisions and their rationale are in [docs/adr/](docs/adr/); what's still
undecided is in [docs/09-open-questions.md](docs/09-open-questions.md).

## Scope (v1)

ECS · SQS · Lambda · DynamoDB · RDS · API Gateway · ElastiCache

Roadmap and what's deferred: [docs/08-roadmap.md](docs/08-roadmap.md).

## Contributing

Design feedback is the most useful contribution right now — particularly on
[the linker](docs/03-linker.md), which is the part most likely to be wrong.
Collectors are the easiest place to start on code.

[CONTRIBUTING.md](CONTRIBUTING.md) covers the setup, the invariants a PR must not
break, and a step-by-step walkthrough for adding a collector.

## License

MIT
