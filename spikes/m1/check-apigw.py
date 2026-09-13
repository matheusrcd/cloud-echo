#!/usr/bin/env python3
"""Field-level checks for the API Gateway topology (12-aws-apigw-create.sh).

Usage: check-apigw.py <inventory.json> [--source aws|floci]
"""
import json, sys
inv = json.load(open(sys.argv[1])); raw = open(sys.argv[1]).read()
source = sys.argv[3] if len(sys.argv) > 3 else "aws"
P = "ce-test"
res = inv["resources"]
by_name = lambda typ, name: next((r for r in res if r["type"] == typ and r["name"] == name), None)
results = []
def check(name, ok, detail=""): results.append((bool(ok), name, detail))

rest, http = by_name("apigateway.rest", f"{P}-orders-rest"), by_name("apigateway.http", f"{P}-orders-http")
check("REST API collected", rest); check("HTTP API collected", http)
if not (rest and http):
    print("cannot continue"); sys.exit(1)
check("ids are API ids, not names", rest["id"] == "apigw/" + rest["spec"]["apiId"] and http["id"] == "apigw/" + http["spec"]["apiId"])
rs, hs = rest["spec"], http["spec"]
rs["routes"] = rs.get("routes") or []; hs["routes"] = hs.get("routes") or []
route = lambda s, k: next((r for r in s["routes"] if r["routeKey"] == k), {})
tgt = lambda r: ((r.get("integration") or {}).get("target") or {}).get("id")

# --- REST
check("REST: 5 routes (root has no methods)", len(rs["routes"]) == 5, str([r["routeKey"] for r in rs["routes"]]))
check("REST GET /orders → lambda, CUSTOM auth", tgt(route(rs, "GET /orders")) == f"lambda/{P}-orders-fn" and route(rs, "GET /orders")["authorization"] == "CUSTOM")
o = route(rs, "GET /orders/{id}")
check("REST GET /orders/{id} → lambda via alias live", tgt(o) == f"lambda/{P}-orders-fn" and o["integration"].get("qualifier") == "live", str(o.get("integration")))
w = route(rs, "POST /webhooks"); wi = w.get("integration") or {}
check("REST POST /webhooks → sqs direct, with role + api key", tgt(w) == f"sqs/{P}-inbox" and wi.get("credentials", "").endswith(f"role/{P}-apigw-sqs-role") and w.get("apiKeyRequired"), str(wi))
check("REST templates withheld, content types kept", wi.get("templateContentTypes") == ["application/json"])
check("REST GET /health → mock", (route(rs, "GET /health").get("integration") or {}).get("service") == "mock")
check("REST GET /legacy → http", (route(rs, "GET /legacy").get("integration") or {}).get("service") == "http")
check("REST resource policy decoded", "execute-api:Invoke" in json.dumps(rs.get("resourcePolicy")), str(rs.get("resourcePolicy"))[:80])
check("REST endpoint", rs.get("endpoint", "").endswith(".execute-api.us-east-2.amazonaws.com") or source == "floci", rs.get("endpoint"))
check("REST TOKEN authorizer → lambda", any((a.get("function") or {}).get("id") == f"lambda/{P}-authorizer-fn" for a in rs.get("authorizers", [])))
check("REST redactions", rs.get("redacted") == ["route:POST /webhooks:integration.request.header.x-api-key", "stage:prod:DB_PASSWORD"], str(rs.get("redacted")))
st = next((s for s in rs.get("stages", []) if s["name"] == "prod"), {})
check("REST stage variable kept when not secret", st.get("variables", {}).get("backend") == f"{P}-orders-fn")

# --- HTTP
a, b = route(hs, "POST /orders"), route(hs, "GET /orders/{id}")
check("HTTP shared integration on two routes", (a.get("integration") or {}).get("id") and a["integration"]["id"] == (b.get("integration") or {}).get("id") and tgt(a) == f"lambda/{P}-orders-fn")
check("HTTP POST /orders JWT", a.get("authorization") == "JWT")
e = (route(hs, "POST /events").get("integration") or {})
check("HTTP POST /events → sqs SendMessage via QueueUrl", e.get("service") == "sqs" and e.get("action") == "SendMessage" and (e.get("target") or {}).get("id") == f"sqs/{P}-inbox", str(e))
check("HTTP $default → http", (route(hs, "$default").get("integration") or {}).get("service") == "http")
check("HTTP JWT authorizer issuer", any(x.get("jwtIssuer") == "https://accounts.google.com" for x in hs.get("authorizers", [])))
check("HTTP redactions", hs.get("redacted") == ["stage:prod:API_TOKEN"], str(hs.get("redacted")))
check("HTTP $default stage autoDeploy", any(s["name"] == "$default" and s.get("autoDeploy") for s in hs.get("stages", [])) or source == "floci")

# --- cross-cutting
for secret in ["not-a-real-upstream-key", "not-a-real-password", "not-a-real-token-123", "MessageBody=$util.urlEncode"]:
    check(f"absent from inventory: {secret[:24]}", raw.count(secret) == 0, f"{raw.count(secret)}x")
role = next((r for r in res if r["id"] == f"iam/role/{P}-apigw-sqs-role"), None)
check("IAM collected the role API Gateway assumes", role is not None)
if role:
    check("…assumed by both APIs, once each", sorted(role["spec"]["assumedBy"]) == sorted([rest["id"], http["id"]]), str(role["spec"]["assumedBy"]))
# Only this topology's warnings: with the M1 topology up too, its planted
# dangling role is reported as well, and is not an API Gateway problem.
kinds = [w["kind"] for w in inv.get("warnings", []) if w.get("service") in ("API Gateway", "ApiGatewayV2") or "apigw" in w.get("message", "")]
check("no API Gateway warnings", not kinds, str(kinds))

width = max(len(n) for _, n, _ in results); fails = 0
for ok, n, d in results:
    fails += not ok; print(f"  {'PASS' if ok else 'FAIL'}  {n:<{width}}  {'' if ok else d}")
print(f"\n{len(results) - fails}/{len(results)} checks passed ({source})"); sys.exit(1 if fails else 0)
