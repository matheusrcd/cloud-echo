#!/usr/bin/env python3
"""Asserts the Tier 1–3 graph of the combined M1 + API Gateway topology.

Written from what 10-aws-create.sh, 12-aws-apigw-create.sh and
13-aws-iam-tier3.sh build, before the linker's output was looked at — each
tier's checks in that tier's round. Usage: check-graph.py <graph.json>
"""
import json, sys
g = json.load(open(sys.argv[1]))
P = "ce-test"
nodes = {n["id"]: n for n in g["nodes"]}
api = {n["name"]: n["id"] for n in g["nodes"] if n["type"].startswith("apigateway.")}
REST, HTTP = api.get(f"{P}-orders-rest"), api.get(f"{P}-orders-http")
edges = {(e["from"], e["to"], e["kind"]): e for e in g["edges"]}
results = []
def check(name, ok, detail=""): results.append((bool(ok), name, detail))
def has(f, t, k, status=""):
    e = edges.get((f, t, k)); return e is not None and e.get("status", "") == status
def rules(f, t, k): return sorted({ev["rule"] for ev in edges.get((f, t, k), {}).get("evidence", [])})
flow = lambda i: nodes.get(i, {}).get("flow")

check("both APIs are nodes", REST and HTTP, str(api))
L, Q, D, X = "lambda/", "sqs/", "ddb/", "ext/api.payments.example.com"
# --- API Gateway: declared targets
check("REST → orders-fn (invoke)", has(REST, L+P+"-orders-fn", "invoke"))
# Tier 2 adds the REST stage variable backend=ce-test-orders-fn, which no
# integration URI uses: a reference absorbed as evidence into this invoke edge.
check("…corroborated by the resource policy and the stage variable", rules(REST, L+P+"-orders-fn", "invoke") == ["apigw.integration", "config.value-scan", "lambda.resource-policy"], str(rules(REST, L+P+"-orders-fn", "invoke")))
check("REST → authorizer-fn (invoke, authorizer)", "apigw.authorizer" in rules(REST, L+P+"-authorizer-fn", "invoke"))
check("REST → inbox (publish, direct SQS)", has(REST, Q+P+"-inbox", "publish"))
check("REST → payments (http, external)", has(REST, X, "http") and nodes.get(X, {}).get("external"))
check("HTTP → orders-fn (invoke)", has(HTTP, L+P+"-orders-fn", "invoke"))
check("HTTP → inbox (publish, SQS-SendMessage)", has(HTTP, Q+P+"-inbox", "publish"))
check("HTTP → payments (http)", has(HTTP, X, "http"))
# --- Lambda / SQS declarations
check("orders-events → order-processor (consume, via alias)", has(Q+P+"-orders-events", L+P+"-order-processor", "consume"))
check("orders → order-processor (consume, disabled)", has(Q+P+"-orders", L+P+"-order-processor", "consume", "disabled"))
check("ddb orders → audit-writer (consume, stream)", has(D+P+"-orders", L+P+"-audit-writer", "consume"))
check("audit-writer → orders-events-dlq (on-failure)", has(L+P+"-audit-writer", Q+P+"-orders-events-dlq", "publish"))
check("webhook-receiver → orders-events-dlq (DLQ config)", has(L+P+"-webhook-receiver", Q+P+"-orders-events-dlq", "publish"))
check("orders-events → orders-events-dlq (redrive)", has(Q+P+"-orders-events", Q+P+"-orders-events-dlq", "publish"))
# --- flow
check("APIs are entrypoints", flow(REST) == "entrypoint" and flow(HTTP) == "entrypoint")
check("orders-fn, authorizer-fn, payments are sync", all(flow(i) == "sync" for i in [L+P+"-orders-fn", L+P+"-authorizer-fn", X]))
check("inbox is async (behind a direct SQS integration)", flow(Q+P+"-inbox") == "async", flow(Q+P+"-inbox"))
wr = nodes.get(L+P+"-webhook-receiver", {})
check("webhook-receiver is an entrypoint: its API is not in the inventory", wr.get("flow") == "entrypoint" and any("not in the inventory" in t for t in wr.get("triggers", [])), str(wr.get("triggers")))
check("orders-events-dlq is async (via webhook-receiver's DLQ)", flow(Q+P+"-orders-events-dlq") == "async", flow(Q+P+"-orders-events-dlq"))
check("ECS services without load balancers are unreached", all(n["flow"] == "unreached" for n in g["nodes"] if n["type"] == "ecs.service"))

# --- Tier 2: configuration values
# Written as evidence claims: Tier 3 folds a reference into the edge that states
# its intent, so "the configuration named it" is checked on whichever edge
# between the pair carries it, in either direction.
def conf(f, t, k="references"): return edges.get((f, t, k), {}).get("confidence")
def findings(kind, part): return [f for f in g.get("findings", []) if f["kind"] == kind and part in f["target"] + f["detail"]]
def between(a, b): return [e for (f, t, _), e in edges.items() if {f, t} == {a, b}]
def named(f, t, var): return any(ev["rule"] == "config.value-scan" and ev["source"].endswith(" " + var)
                                 for e in between(f, t) for ev in e["evidence"])
SVC = [n["id"] for n in g["nodes"] if n["type"] == "ecs.service"]
API_SVC = [i for i in SVC if i.endswith("/" + P + "-orders-api")]
NOTIF = [i for i in SVC if i.endswith(P + "-notifications") or "-filler-" in i]
check("setup: orders-api runs in two clusters, notifications' task def in 12 services", len(API_SVC) == 2 and len(NOTIF) == 12, f"{API_SVC} {len(NOTIF)}")
# The pipeline the Tier-1 round left unreached, connected by the entrypoint that names its queue.
check("webhook-receiver names orders-events (ORDERS_QUEUE_URL)", named(L+P+"-webhook-receiver", Q+P+"-orders-events", "ORDERS_QUEUE_URL"))
check("the ESM pipeline is now async: orders-events, order-processor", flow(Q+P+"-orders-events") == "async" and flow(L+P+"-order-processor") == "async",
      f"{flow(Q+P+'-orders-events')} {flow(L+P+'-order-processor')}")
check("both orders-api services name orders-events (QUEUE_URL)", all(named(s, Q+P+"-orders-events", "QUEUE_URL") for s in API_SVC))
check("both orders-api services → payments (PAYMENTS_URL, http high)", all(conf(s, X, "http") == "high" for s in API_SVC))
check("12 services name notifications.fifo (QUEUE_URL)", all(named(s, Q+P+"-notifications.fifo", "QUEUE_URL") for s in NOTIF))
# The planted ambiguity: a table and a queue named ce-test-orders.
holders = API_SVC + [L+P+"-order-processor"]
check("TABLE_NAME=ce-test-orders is read as the table (the key names a table)", all(named(h, D+P+"-orders", "TABLE_NAME") for h in holders))
check("…and the queue only as a low candidate", all(conf(h, Q+P+"-orders") == "low" for h in holders), str([conf(h, Q+P+"-orders") for h in holders]))
check("…with the ambiguity reported for each holder", sorted(f["node"] for f in findings("ambiguous", P + "-orders")) == sorted(holders))
check("a low candidate drives no flow: the queue ce-test-orders stays unreached", flow(Q+P+"-orders") == "unreached", flow(Q+P+"-orders"))
check("audit-writer names orders-audit (AUDIT_TABLE)", named(L+P+"-audit-writer", D+P+"-orders-audit", "AUDIT_TABLE"))
# The Tier-2 inputs 12-aws-apigw-create.sh sets on orders-fn, which is on the request path.
check("orders-fn → orders-audit (AUDIT_TABLE_ARN, high)", conf(L+P+"-orders-fn", D+P+"-orders-audit") == "high")
check("…so orders-audit is sync (sync takes precedence over the stream path)", flow(D+P+"-orders-audit") == "sync", flow(D+P+"-orders-audit"))
check("orders-fn → HTTP API (PUBLIC_API_URL, http high)", conf(L+P+"-orders-fn", HTTP, "http") == "high")
check("PARTNER_QUEUE_URL: a namesake queue in another account is reported…", findings("unresolved", ":999999999999:" + P + "-orders-events"))
check("…and not linked to the local queue", not any(f == L+P+"-orders-fn" and t == Q+P+"-orders-events" for f, t, _ in edges))
# Third parties, and what must not become one.
S = "ext/hooks.slack.com"
check("order-processor → hooks.slack.com (webhook path withheld, host kept)", conf(L+P+"-order-processor", S, "http") == "high")
check("…async: it is called beside the request path", flow(S) == "async", flow(S))
ext = sorted(n["id"] for n in g["nodes"] if n.get("external"))
check("exactly two third parties; no AWS host became one", ext == [X, S], str(ext))
# With the RDS collector, only orders-api's made-up host stays unresolved;
# order-processor's DATABASE_URL now names the real cluster (checked below).
check("the only unresolved RDS endpoints are orders-api's",
      sorted({f["node"] for f in findings("unresolved", ".rds.amazonaws.com")}) == sorted(API_SVC))
check("the ElastiCache endpoint reported for all 12 services", len({f["node"] for f in findings("unresolved", ".cache.amazonaws.com")}) == 12)
check("every environment was readable", not [f for f in g.get("findings", []) if f["kind"] == "unscanned" and f["rule"] == "config.value-scan"])
refs = [e for e in g["edges"] if any(ev["rule"] == "config.value-scan" for ev in e["evidence"])]
check("every config edge names the variable it came from", refs and all(
    any(ev["rule"] == "config.value-scan" and (" env " in ev["source"] or " variable " in ev["source"]) for ev in e["evidence"]) for e in refs))
# Configuration states a verb only where the value allows one: a URL is for
# calling (http), a database endpoint for connecting (connect, since the RDS
# round). Never publish, consume, read or write.
check("no edge's intent rests on configuration alone", all(e["kind"] in ("references", "http", "connect") or
    any(ev["rule"] != "config.value-scan" for ev in e["evidence"]) for e in refs))

# --- Tier 3: what roles permit (10-aws-create.sh roles, 13-aws-iam-tier3.sh additions)
IAM = "iam.policy-resource"
def iam(f, t, k): return IAM in rules(f, t, k)
check("orders-api reads and writes the orders table, high: config and role agree",
      all(conf(s, D+P+"-orders", k) == "high" and iam(s, D+P+"-orders", k) for s in API_SVC for k in ("read", "write")),
      str([(conf(s, D+P+"-orders", "read"), conf(s, D+P+"-orders", "write")) for s in API_SVC]))
check("orders-api publishes to orders-events, high (QUEUE_URL + SendMessage)",
      all(conf(s, Q+P+"-orders-events", "publish") == "high" and iam(s, Q+P+"-orders-events", "publish") for s in API_SVC))
check("the ambiguity is settled: orders-api's role cannot touch the queue ce-test-orders",
      sorted(f["node"] for f in findings("unpermitted", Q+P+"-orders") if f["node"] in API_SVC) == sorted(API_SVC))
check("the boundary cancels orders-api's InvokeFunction: no edge, a blocked finding each",
      not any(has(s, L+P+"-orders-fn", "invoke") for s in API_SVC)
      and sorted(f["node"] for f in findings("blocked", "boundary") if f["target"] == L+P+"-orders-fn") == sorted(API_SVC))
check("webhook-receiver publishes to orders-events, high (ORDERS_QUEUE_URL + SendMessage)",
      conf(L+P+"-webhook-receiver", Q+P+"-orders-events", "publish") == "high")
check("the explicit Deny cancels webhook-receiver's PutItem: no edge, a blocked finding",
      not has(L+P+"-webhook-receiver", D+P+"-orders", "write")
      and any(f["node"] == L+P+"-webhook-receiver" for f in findings("blocked", "explicit Deny")))
check("Q17: 12 workers poll notifications.fifo — consume, high, and no reference left pointing the other way",
      all(conf(Q+P+"-notifications.fifo", s, "consume") == "high" and conf(s, Q+P+"-notifications.fifo") is None for s in NOTIF))
check("…and AmazonSQSFullAccess is reported as broad for all 12, drawing nothing",
      sorted(f["node"] for f in findings("broad-access", "AmazonSQSFullAccess")) == sorted(NOTIF))
check("order-processor writes the orders table, high (TABLE_NAME + UpdateItem)", conf(L+P+"-order-processor", D+P+"-orders", "write") == "high")
# The pattern alone gives both reads low; TABLE_NAME names one of its matches,
# and a reference folds into every same-direction edge — so the table the
# configuration picks is medium, and the one it does not stays a candidate.
check("table/ce-test-orders* matches two tables: the one TABLE_NAME names is medium, the other low",
      conf(L+P+"-order-processor", D+P+"-orders", "read") == "medium" and conf(L+P+"-order-processor", D+P+"-orders-audit", "read") == "low")
check("the disabled mapping stays disabled, though the role may still receive",
      has(Q+P+"-orders", L+P+"-order-processor", "consume", "disabled") and iam(Q+P+"-orders", L+P+"-order-processor", "consume"))
check("the Tier-1 consumers are corroborated by their roles (mapping, stream, on-failure)",
      iam(Q+P+"-orders-events", L+P+"-order-processor", "consume") and iam(D+P+"-orders", L+P+"-audit-writer", "consume")
      and iam(L+P+"-audit-writer", Q+P+"-orders-events-dlq", "publish"))
check("audit-writer writes orders-audit, high (AUDIT_TABLE + PutItem)", conf(L+P+"-audit-writer", D+P+"-orders-audit", "write") == "high")
check("orders-fn names orders-audit but its role cannot touch it: unpermitted",
      any(f["node"] == L+P+"-orders-fn" for f in findings("unpermitted", D+P+"-orders-audit")))
check("both APIs' SQS integrations are corroborated by the role they assume",
      iam(REST, Q+P+"-inbox", "publish") and iam(HTTP, Q+P+"-inbox", "publish"))
check("an API gains nothing from its role alone", not any(e["from"] in (REST, HTTP) and e["evidence"] and
      all(ev["rule"] == IAM for ev in e["evidence"]) for e in g["edges"]))
check("the workload whose role was deleted is reported, not silently unevaluated",
      any(f["node"] == L+P+"-legacy-report" for f in findings("unscanned", "role not in the inventory")))
check("a role alone never makes an edge more than medium", all(e["confidence"] in ("medium", "low") for e in g["edges"]
      if e["evidence"] and all(ev["rule"] == IAM for ev in e["evidence"])))
check("no edge from a broad grant: nothing IAM-only leaves the 12 notification services",
      not any(e["from"] in NOTIF and all(ev["rule"] == IAM for ev in e["evidence"]) for e in g["edges"]))

# --- RDS (14-aws-rds-create.sh): written before the RDS round's output was looked at
CL, LG = "rds/cluster/" + P + "-db", "rds/" + P + "-legacy-db"
check("RDS: the cluster and the instance are nodes; the Aurora member is not",
      nodes.get(CL, {}).get("type") == "rds.cluster" and nodes.get(LG, {}).get("type") == "rds.instance"
      and not any(i.endswith(P + "-db-instance-1") for i in nodes))
check("order-processor connects to the cluster, high: DATABASE_URL (writer) + the Data API grant",
      conf(L+P+"-order-processor", CL, "connect") == "high" and rules(L+P+"-order-processor", CL, "connect") == ["config.value-scan", IAM],
      str(rules(L+P+"-order-processor", CL, "connect")))
check("orders-fn connects to the cluster through its reader endpoint (DB_READER_HOST)",
      named(L+P+"-orders-fn", CL, "DB_READER_HOST") and conf(L+P+"-orders-fn", CL, "connect") == "high")
check("orders-fn connects to the instance, high: its master secret's ARN + GetSecretValue on it",
      conf(L+P+"-orders-fn", LG, "connect") == "high" and rules(L+P+"-orders-fn", LG, "connect") == ["config.value-scan", IAM])
check("authorizer-fn shares the role: connects to the instance at medium, from the grant alone",
      conf(L+P+"-authorizer-fn", LG, "connect") == "medium")
check("webhook-receiver connects to the instance (LEGACY_DB_URL, password redacted, host kept)",
      named(L+P+"-webhook-receiver", LG, "LEGACY_DB_URL") and conf(L+P+"-webhook-receiver", LG, "connect") == "high")
check("orders-api's ce-test-db.cluster-abc123…: the cluster's name, another account's suffix — reported, not linked",
      not any(has(s, CL, "connect") for s in API_SVC)
      and sorted({f["node"] for f in findings("unresolved", "namesake") if "cluster-abc123" in f["target"]}) == sorted(API_SVC))
check("both databases are on the request path (sync)", flow(CL) == "sync" and flow(LG) == "sync", f"{flow(CL)} {flow(LG)}")
check("no database is called unpermitted", not [f for f in g.get("findings", []) if f["kind"] == "unpermitted" and f["target"].startswith("rds/")])

# --- ElastiCache (15-aws-elasticache-create.sh): written before the round's output was looked at
SG_, SL_, MC_ = "cache/" + P + "-sessions", "cache/serverless/" + P + "-ratelimit", "cache/cluster/" + P + "-memo"
check("caches: one node per API, the group's member is not one",
      all(nodes.get(i, {}).get("type") == "elasticache.cache" for i in (SG_, SL_, MC_)) and not any(i.endswith("-sessions-001") for i in nodes))
check("order-processor connects to the group, high: SESSIONS_URL (rediss://, primary) + elasticache:Connect",
      conf(L+P+"-order-processor", SG_, "connect") == "high" and rules(L+P+"-order-processor", SG_, "connect") == ["config.value-scan", IAM],
      str(rules(L+P+"-order-processor", SG_, "connect")))
check("orders-fn connects to the group through its reader endpoint (SESSIONS_READER)",
      named(L+P+"-orders-fn", SG_, "SESSIONS_READER") and conf(L+P+"-orders-fn", SG_, "connect") == "high"
      and any("reader endpoint" in ev["detail"] for ev in edges.get((L+P+"-orders-fn", SG_, "connect"), {}).get("evidence", [])))
check("orders-fn connects to the serverless cache (RATE_LIMIT_HOST)", named(L+P+"-orders-fn", SL_, "RATE_LIMIT_HOST")
      and conf(L+P+"-orders-fn", SL_, "connect") == "high")
check("webhook-receiver connects to memcached through its configuration endpoint (MEMO_SERVERS, host:port)",
      named(L+P+"-webhook-receiver", MC_, "MEMO_SERVERS") and conf(L+P+"-webhook-receiver", MC_, "connect") == "high")
check("the 12 services' CACHE_HOST — the group's name, another suffix — a namesake each, never linked",
      not any(has(s, SG_, "connect") for s in NOTIF)
      and sorted({f["node"] for f in findings("unresolved", "namesake") if ".cache.amazonaws.com" in f["target"]}) == sorted(NOTIF))
check("all three caches are on the request path (sync)", all(flow(i) == "sync" for i in (SG_, SL_, MC_)), str([flow(i) for i in (SG_, SL_, MC_)]))
check("no cache is called unpermitted", not [f for f in g.get("findings", []) if f["kind"] == "unpermitted" and f["target"].startswith("cache/")])

# --- hygiene
check("every edge has evidence", all(e["evidence"] for e in g["edges"]))
check("no edge touches a non-node", all(e["from"] in nodes and e["to"] in nodes for e in g["edges"]))
stale = [f for f in g.get("findings", []) if f["kind"] == "stale-permission"]
check("no stale permissions (every granted API really routes to its function)", not stale, str(stale))
check("no warnings", not g.get("warnings"), str(g.get("warnings")))

w = max(len(n) for _, n, _ in results); fails = 0
for ok, n, d in results:
    fails += not ok; print(f"  {'PASS' if ok else 'FAIL'}  {n:<{w}}  {'' if ok else d}")
print(f"\n{len(results)-fails}/{len(results)} checks passed"); sys.exit(1 if fails else 0)
