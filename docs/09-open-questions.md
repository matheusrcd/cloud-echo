# 09 — Open Questions

Unresolved decisions. Each should eventually become an ADR or be explicitly
dropped. Ordered roughly by how much they'd hurt if answered wrong.

---

### ~~Q1 — How complete is Floci's ECS?~~ · CLOSED 2026-08-27

**Answered by M0.** Real containers ✅, env injection ✅, correct network ✅ —
`runtime: floci-ecs` stays the default. Task metadata v4 ❌, role credential
vending ❌, and `CreateService` never launches tasks ❌ (use `RunTask`).

Full results and consequences:
[spikes/m0-findings.md](spikes/m0-findings.md).

---

### Q11 — Floci persistence is intermittent

**Raised by M0 (D7).** Isolated tests pass consistently; the full suite run lost
all DynamoDB state on restart, and one restart logged
`Loaded 0 entries from dynamodb-tables.json` against a file holding 2449 bytes of
valid JSON.

Leading hypothesis: a race between the readiness signal and the storage load —
`sts:GetCallerIdentity` answers before state finishes loading. **Not isolated.**

Mitigated by design already (idempotent seeders, sentinel check after readiness),
so it does not block M2. But it also means the readiness signal itself may not be
trustworthy in general, which is worth pinning down before M2 ships.

---

### Q12 — Report the Docker socket bug upstream

**Raised by M0 (D6).** Floci's docker-java client cannot open a mounted unix
socket on Docker Desktop for macOS (`BindException: Permission denied` against
`unix://localhost:2375`). Reproduction is small.

Two things worth reporting: the bug itself, and the misleading diagnostic — RDS
surfaces `Failed to remove stale container`, which points nowhere near the cause
and cost real debugging time.

Ties into [Q9](#q9--relationship-with-floci-upstream): a good first contribution.

---

### Q13 — Lambda aliases and versions

**Raised by M1.** The inventory records `$LATEST` and, on event source mappings,
the qualifier they target (`live`) — but not which version an alias points at or
what that version's configuration is. The round-trip seeder had to invent the
alias. Where production traffic goes through an alias pinned to an older version,
the locally recreated function can differ from what actually runs.

Options: collect aliases (`lambda:ListAliases`) and the configuration of each
aliased version (`GetFunctionConfiguration` with a qualifier) — two more
permissions — or treat `$LATEST` as the local truth and flag qualified targets.
Leaning toward the second for v1, with a visible warning when a mapping or
integration targets a qualifier whose version differs from `$LATEST`.

---

### Q14 — What `scan --diff` must ignore

**Raised by M1.** Two scans of an unchanged real account minutes apart were
identical once scan id and per-resource `collectedAt` were set aside — Raw
included. So determinism holds, but a byte-level diff will always differ on those
fields. `scan --diff` needs an explicit list of volatile fields, and fields like
DynamoDB's `itemCountEstimate` or ECS `runningCount` will need the same treatment
in a busier account.

---

### Q15 — API Gateway: deployed definition and custom domains

**Raised by the API Gateway collector.** Two things it deliberately does not do:

- **Deployed vs current definition.** Routes come from the API's current
  definition. A REST stage serves a *deployment* snapshot, which lags behind
  undeployed console edits. `GetExport` per stage would return what is actually
  live, at the cost of parsing an OpenAPI document per stage.
- **Custom domains.** `https://api.company.com` in another service's env var
  links to an API only through a domain mapping (`GetDomainNames` +
  `GetBasePathMappings` / `GetApiMappings`). Those paths are outside the scoped
  policy ([ADR-0008](adr/0008-scope-coarse-iam-actions.md)); collecting them means
  widening it to `/domainnames*` on purpose.

Leaning: custom domains first — internal callers commonly use them, and the
Tier-2 link is otherwise invisible — with the scope widened explicitly.

---

### Q2 — Stateful mocks

Sequence-dependent responses ("first poll returns `pending`, second returns
`complete`") are a real testing need, and the polling-until-ready pattern is
everywhere.

Options: a `state:` machine per integration; a `times:`/`then:` field on rules; or
an escape hatch to a user-supplied JS/Lua handler.

A scripting escape hatch is the most powerful and the most dangerous — it makes
mocks unreviewable and turns the gateway into a runtime. Leaning toward a
declarative `sequence:` for v1 and revisiting only if it proves insufficient.

---

### Q3 — Interception of legacy SDKs and hardcoded URLs

`AWS_ENDPOINT_URL` covers modern SDKs. It does not cover AWS SDK JS v2, Java v1,
or a hardcoded `https://sqs.us-east-1.amazonaws.com/...`.

The DNS-override fallback is more complete but adds a resolver container and
mandatory TLS interception for all AWS traffic. Is the extra fidelity worth the
extra failure mode? Probably yes eventually, behind a flag. Needs data on how
common legacy SDKs actually are among target users.

---

### Q4 — Blueprint versioning and migration

The schema will change. Migrating a user's committed `cloud-echo.yaml` across
versions needs a story before v1.0, not after.

Leaning: `version:` field plus in-place migrations run by `cloud-echo migrate`,
with a hard error (never a silent upgrade) when a newer binary reads an older
blueprint.

---

### Q5 — Naming and ID stability · PARTIALLY ANSWERED

Resource IDs must be stable across scans, or every re-plan produces a meaningless
diff and overrides detach from their targets.

Name-based IDs break when a resource is renamed. ARN-based IDs are stable but ugly
and leak the account ID into the committed file. Current leaning: name-based ID
with the ARN recorded in `origin`, and a rename-detection pass on re-plan that
proposes an ID remap rather than silently creating a new node.

**Settled for ECS (M1):** IDs are scoped by whatever AWS uses to enforce
uniqueness — `ecs/<cluster>/<service>`, not `ecs/<service>`, because service names
are only unique within a cluster. A "shorten it when unambiguous" scheme was
considered and rejected: it makes an existing ID change when an unrelated
colliding resource appears elsewhere, which is precisely the instability this
question is about.

Still open: rename detection, and whether every service can be scoped this
cleanly. Some (Lambda, DynamoDB) are account-region unique and need no scope at
all, so the ID shape will not be uniform across services — that is fine, but it
should be a documented rule per service rather than a per-collector improvisation.

---

### Q6 — How much of IAM to emulate

Floci emulates IAM. Should locally-created roles carry the real policies?

- **For:** catches "the app assumed it could write to this table" bugs.
- **Against:** local IAM enforcement is not faithful enough to trust, so a failure
  might be Floci's rather than the app's — which is worse than no signal at all.

Leaning: create the roles with real policies for shape and inspectability, but do
not rely on enforcement, and say so in the docs.

**Evidence from M1 (2026-09-13):** Floci's IAM is shape-only. Permissions
boundaries are not returned, and AWS-managed policies exist as stubs granting
`Action: "*"` — a role with `AWSLambdaBasicExecutionRole` can do anything
locally. That settles it in practice: local IAM cannot be a source of truth, so
the leaning stands and "not enforced" is the documented behaviour.

---

### Q7 — RDS schema without production access

`data.mode: none` gives an empty Postgres. Most applications will not start
against an empty database.

Options: user-supplied migrations (works, but only if the team has them in a
runnable form), schema-only dump (needs real DB access — outside the scanner's
permissions and network reach), or a "connect once to a non-prod instance and dump
the schema" side command.

The last is probably the honest answer, as a distinctly-permissioned opt-in
command. It is not part of `scan`.

---

### Q8 — Project name collision

Verify `cloud-echo` is free on GitHub, Homebrew, and the Go module namespace
before the name spreads through docs and imports.

---

### Q9 — Relationship with Floci upstream

Some of this — particularly resource seeding — could reasonably live in Floci.
Worth talking to the Floci maintainers early: the wrong outcome is building a
scanner that duplicates something they're already shipping.

Also worth asking whether they want a "seed from spec" API, which would let
cloud-echo hand over a manifest instead of making N SDK calls.

---

### Q10 — Distribution of the UI

Embedding a pre-built React bundle in the repo means committing build artifacts,
which is ugly but keeps `go build` self-sufficient for Go-only contributors.

Alternative: build the UI in CI and attach it to releases, with `go build` from a
clean checkout producing a binary whose `ui` command says "not built."

Leaning toward the CI approach with a clear error message — committed `dist/`
directories rot and produce confusing diffs.
