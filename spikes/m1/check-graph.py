#!/usr/bin/env python3
"""Asserts the Tier-1 graph of the combined M1 + API Gateway topology.

Written from what 10-aws-create.sh and 12-aws-apigw-create.sh build, before the
linker's output was looked at. Usage: check-graph.py <graph.json>
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
check("…corroborated by the function's resource policy", rules(REST, L+P+"-orders-fn", "invoke") == ["apigw.integration", "lambda.resource-policy"], str(rules(REST, L+P+"-orders-fn", "invoke")))
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
check("the ESM pipeline is unreached at Tier 1 (its producer writes through the SDK)", flow(Q+P+"-orders-events") == "unreached" and flow(L+P+"-order-processor") == "unreached")
check("ECS services without load balancers are unreached", all(n["flow"] == "unreached" for n in g["nodes"] if n["type"] == "ecs.service"))
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
