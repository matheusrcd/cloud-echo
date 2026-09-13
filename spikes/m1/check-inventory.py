#!/usr/bin/env python3
"""Asserts that an inventory scanned from the M1 topology says what was planted.

Usage: check-inventory.py <inventory.json> [--source aws|floci]

Every check names the property it protects. Run against the real account and
against Floci, the same checks should hold — except where noted, and those
exceptions are findings, not bugs in this script.
"""
import json, sys

path = sys.argv[1]
source = sys.argv[3] if len(sys.argv) > 3 and sys.argv[2] == "--source" else "aws"
raw = open(path).read()
inv = json.loads(raw)
by_id = {r["id"]: r for r in inv["resources"]}
spec = lambda i: by_id[i]["spec"]

results = []
def check(name, ok, detail=""):
    results.append((ok, name, detail))

P = "ce-test"

# --- redaction: planted secrets must not appear anywhere, Raw included
for secret in ["not-a-real-password", "not-a-real-payments-key-7f3a9c", "not-a-real-payments-token-0000",
               "not-a-real-smtp-pass", "Xq7Lm2Pz9Rt4Wn6Ks1Vb8Hd3"]:
    n = raw.count(secret)
    check(f"secret {secret[:18]}… absent from inventory (spec + raw)", n == 0, f"{n} occurrence(s)")
check("RDS host survives redaction of DATABASE_URL", f"@{P}-db.cluster-abc123" in raw)
check("webhook host survives redaction", "https://hooks.slack.com/services/T-FAKE/B-FAKE/<redacted:url-path>" in raw)

# --- ECS
main = [i for i in by_id if i.startswith(f"ecs/{P}-main/")]
check("ListServices paginated: 13 services in ce-test-main (page size 10)", len(main) == 13, f"got {len(main)}")
a, b = f"ecs/{P}-main/{P}-orders-api", f"ecs/{P}-batch/{P}-orders-api"
check("same-named services in two clusters stay distinct", a in by_id and b in by_id)
tds = sorted(i for i in by_id if i.startswith("ecs/taskdef/"))
# Revisions are derived, not written down: AWS never reuses a revision number,
# so a torn-down and recreated topology runs :3 where the first one ran :2.
in_use = sorted({r["spec"]["taskDefinitionId"] for r in by_id.values() if r["type"] == "ecs.service"})
check("task definitions: exactly one per distinct revision in use", tds == in_use, f"{tds} vs {in_use}")
td = spec(spec(a)["taskDefinitionId"])
check("task role id drops the IAM path", td.get("taskRoleId") == f"iam/role/{P}-orders-api-task", td.get("taskRoleId", ""))
app = td["containers"][0]
check("redacted list names what was removed", app.get("redacted") == ["env:DATABASE_URL", "env:PAYMENTS_API_KEY"], str(app.get("redacted")))
check("secrets[] recorded as ARN, not value", app.get("secrets", {}).get("DB_PASSWORD", "").startswith("arn:aws:secretsmanager:"))
check("sidecar non-essential", td["containers"][1]["essential"] is False)
nt = spec(spec(f"ecs/{P}-main/{P}-notifications")["taskDefinitionId"])["containers"][0]
check("command-line secret redacted", nt.get("redacted") == ["command[2]"], str(nt.get("redacted")))

check("dependsOn keeps its condition", app.get("dependsOn") == [{"container": "otel", "condition": "START"}], str(app.get("dependsOn")))
check("full log configuration recorded", (app.get("log") or {}).get("driver") == "awslogs"
      and (app.get("log") or {}).get("options", {}).get("awslogs-stream-prefix") == "ecs", str(app.get("log")))

# --- SQS
oe = spec(f"sqs/{P}-orders-events")
check("redrive parsed and resolved to the DLQ id", (oe.get("redrive") or {}).get("deadLetterTargetId") == f"sqs/{P}-orders-events-dlq"
      and oe["redrive"]["maxReceiveCount"] == 5, str(oe.get("redrive")))
check("queue access policy kept", "orders-api-task" in json.dumps(oe.get("policy")))
fifo = spec(f"sqs/{P}-notifications.fifo")
check("FIFO + content dedup", fifo.get("fifo") and fifo.get("contentBasedDeduplication"))

# --- DynamoDB
t = spec(f"ddb/{P}-orders")
check("key schema joined with attribute types", t["keySchema"] == [{"name": "pk", "type": "S", "role": "HASH"}, {"name": "sk", "type": "S", "role": "RANGE"}])
check("GSI range key keeps its own type (N)", t["globalSecondaryIndexes"][0]["keySchema"][1] == {"name": "createdAt", "type": "N", "role": "RANGE"})
check("stream recorded", (t.get("stream") or {}).get("viewType") == "NEW_AND_OLD_IMAGES")
check("TTL recorded (separate API)", (t.get("ttl") or {}).get("attribute") == "expiresAt", str(t.get("ttl")))
check("provisioned billing mode", spec(f"ddb/{P}-orders-audit")["billingMode"] == "PROVISIONED")
check("provisioned throughput in spec", spec(f"ddb/{P}-orders-audit").get("provisionedThroughput") == {"read": 1, "write": 1}, str(spec(f"ddb/{P}-orders-audit").get("provisionedThroughput")))
check("on-demand table reports no capacity", "provisionedThroughput" not in t)
check("ambiguity planted: table and queue share a name", f"ddb/{P}-orders" in by_id and f"sqs/{P}-orders" in by_id)

# --- Lambda
fn = spec(f"lambda/{P}-order-processor")
check("lambda env redaction", fn.get("redacted") == ["env:DATABASE_URL", "env:PAYMENTS_TOKEN", "env:SLACK_WEBHOOK_URL"], str(fn.get("redacted")))
check("lambda role id", fn.get("roleId") == f"iam/role/{P}-order-processor-role")
wh = spec(f"lambda/{P}-webhook-receiver")
check("resource policy names apigateway", "apigateway.amazonaws.com" in json.dumps(wh.get("resourcePolicy")))
check("async DLQ resolved", (wh.get("deadLetter") or {}).get("id") == f"sqs/{P}-orders-events-dlq", str(wh.get("deadLetter")))
esms = {r["spec"]["source"]["id"] + "->" + r["spec"].get("functionId", ""): r["spec"] for r in inv["resources"] if r["type"] == "lambda.event-source-mapping"}
e1 = esms.get(f"sqs/{P}-orders-events->lambda/{P}-order-processor", {})
check("ESM to alias keeps qualifier", e1.get("qualifier") == "live", str(e1.get("qualifier")))
check("ESM filter recorded", bool(e1.get("filters")))
check("ESM enabled (state " + str(e1.get("state")) + ")", e1.get("enabled") is True)
e2 = esms.get(f"ddb/{P}-orders->lambda/{P}-audit-writer", {})
check("stream ESM resolves to table + on-failure DLQ", e2.get("sourceType") == "dynamodb-stream" and (e2.get("onFailure") or {}).get("id") == f"sqs/{P}-orders-events-dlq", str(e2.get("onFailure")))
e3 = esms.get(f"sqs/{P}-orders->lambda/{P}-order-processor", {})
check("disabled ESM reads as not enabled (state " + str(e3.get("state")) + ")", e3.get("enabled") is False)

# --- IAM
roles = sorted(i for i in by_id if i.startswith("iam/role/"))
# Every role is there because a workload or an API integration assumes it; the
# count is not written down, since each round's topology adds its own.
check("only assumed roles collected (no execution role)", f"iam/role/{P}-ecs-exec-role" not in by_id
      and roles and all(spec(i).get("assumedBy") for i in roles), str(roles))
r = spec(f"iam/role/{P}-orders-api-task")
check("role path recorded, not in id", r.get("path") == "/ce-test/")
check("permissions boundary recorded", (r.get("permissionsBoundary") or {}).get("id") == f"iam/policy/{P}-boundary")
check("assumedBy links back to the task definition", r.get("assumedBy") == [spec(a)["taskDefinitionId"]], str(r.get("assumedBy")))
obs = spec(f"iam/policy/{P}-observability")
check("policy document URL-decoded ('+' and space)", '"orders+payments team"' in json.dumps(obs["document"]))
check("AWS-managed policy id namespace", "iam/aws-policy/AWSLambdaBasicExecutionRole" in by_id)
check("explicit Deny kept", '"Deny"' in json.dumps(spec(f"iam/role/{P}-webhook-receiver-role")["inlinePolicies"]))
check("IAM region is global", by_id[f"iam/role/{P}-orders-api-task"]["region"] == "global")
kinds = sorted(w["kind"] for w in inv.get("warnings", []))
check("dangling role reported", "dangling-reference" in kinds, str(kinds))

# --- output
width = max(len(n) for _, n, _ in results)
fails = 0
for ok, name, detail in results:
    fails += not ok
    print(f"  {'PASS' if ok else 'FAIL'}  {name:<{width}}  {'' if ok else detail}")
print(f"\n{len(results) - fails}/{len(results)} checks passed ({source})")
sys.exit(1 if fails else 0)
