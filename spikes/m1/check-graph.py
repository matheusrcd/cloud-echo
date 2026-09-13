#!/usr/bin/env python3
"""Asserts the Tier-1 and Tier-2 graph of the combined M1 + API Gateway topology.

Written from what 10-aws-create.sh and 12-aws-apigw-create.sh build, before the
linker's output was looked at — the Tier-1 checks in the Tier-1 round, the
Tier-2 checks in the Tier-2 round. Usage: check-graph.py <graph.json>
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
def conf(f, t, k="references"): return edges.get((f, t, k), {}).get("confidence")
def findings(kind, part): return [f for f in g.get("findings", []) if f["kind"] == kind and part in f["target"] + f["detail"]]
SVC = [n["id"] for n in g["nodes"] if n["type"] == "ecs.service"]
API_SVC = [i for i in SVC if i.endswith("/" + P + "-orders-api")]
NOTIF = [i for i in SVC if i.endswith(P + "-notifications") or "-filler-" in i]
check("setup: orders-api runs in two clusters, notifications' task def in 12 services", len(API_SVC) == 2 and len(NOTIF) == 12, f"{API_SVC} {len(NOTIF)}")
# The pipeline the Tier-1 round left unreached, connected by the entrypoint that names its queue.
check("webhook-receiver → orders-events (ORDERS_QUEUE_URL, high)", conf(L+P+"-webhook-receiver", Q+P+"-orders-events") == "high")
check("the ESM pipeline is now async: orders-events, order-processor", flow(Q+P+"-orders-events") == "async" and flow(L+P+"-order-processor") == "async",
      f"{flow(Q+P+'-orders-events')} {flow(L+P+'-order-processor')}")
check("a queue URL is a reference, never a publish", not any(has(f, Q+P+"-orders-events", "publish") for f in [L+P+"-webhook-receiver"] + API_SVC))
check("both orders-api services → orders-events (QUEUE_URL, high)", all(conf(s, Q+P+"-orders-events") == "high" for s in API_SVC))
check("both orders-api services → payments (PAYMENTS_URL, http high)", all(conf(s, X, "http") == "high" for s in API_SVC))
check("12 services → notifications.fifo (QUEUE_URL, high)", all(conf(s, Q+P+"-notifications.fifo") == "high" for s in NOTIF))
# The planted ambiguity: a table and a queue named ce-test-orders.
holders = API_SVC + [L+P+"-order-processor"]
check("TABLE_NAME=ce-test-orders → the table, medium (the key names a table)", all(conf(h, D+P+"-orders") == "medium" for h in holders),
      str([conf(h, D+P+"-orders") for h in holders]))
check("…and the queue only as a low candidate", all(conf(h, Q+P+"-orders") == "low" for h in holders), str([conf(h, Q+P+"-orders") for h in holders]))
check("…with the ambiguity reported for each holder", sorted(f["node"] for f in findings("ambiguous", P + "-orders")) == sorted(holders))
check("a low candidate drives no flow: the queue ce-test-orders stays unreached", flow(Q+P+"-orders") == "unreached", flow(Q+P+"-orders"))
check("audit-writer → orders-audit (AUDIT_TABLE, medium)", conf(L+P+"-audit-writer", D+P+"-orders-audit") == "medium")
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
check("RDS endpoints reported for both orders-api services and order-processor",
      sorted({f["node"] for f in findings("unresolved", ".rds.amazonaws.com")}) == sorted(holders))
check("the ElastiCache endpoint reported for all 12 services", len({f["node"] for f in findings("unresolved", ".cache.amazonaws.com")}) == 12)
check("no blind spots: every environment was readable", not findings("unscanned", ""), str(findings("unscanned", "")))
refs = [e for e in g["edges"] if any(ev["rule"] == "config.value-scan" for ev in e["evidence"])]
check("every config edge names the variable it came from", refs and all(
    any(ev["rule"] == "config.value-scan" and (" env " in ev["source"] or " variable " in ev["source"]) for ev in e["evidence"]) for e in refs))
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
