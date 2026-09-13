#!/usr/bin/env python3
"""Asserts the SNS part of an inventory scanned from the M1 topology.

Written from what 17-aws-sns-create.sh builds, before the collector's output
was looked at. The planted basic-auth password and query token are read from
.work/sns-planted.json (gitignored). Usage: check-sns.py <inventory.json>
"""
import json, os, sys
path = sys.argv[1]
inv = json.load(open(path))
text = open(path).read()
P = "ce-test"
by_id = {r["id"]: r for r in inv["resources"]}
results = []
def check(name, ok, detail=""): results.append((bool(ok), name, detail))
spec = lambda i: by_id.get(i, {}).get("spec", {})
def subs(t, proto): return [s for s in spec(t).get("subscriptions", []) if s["protocol"] == proto]

OT, AL, FI = f"sns/{P}-orders-topic", f"sns/{P}-alerts", f"sns/{P}-notifications.fifo"
check("three topics", sorted(i for i in by_id if i.startswith("sns/")) == sorted([OT, AL, FI]), str(sorted(i for i in by_id if i.startswith("sns/"))))
check("tags, from ListTagsForResource", all(by_id.get(i, {}).get("tags", {}).get("cloud-echo-test") == "m1" for i in [OT, AL, FI]))
# --- orders-topic
check("orders-topic: three subscriptions — sqs, lambda, https", sorted(s["protocol"] for s in spec(OT).get("subscriptions", [])) == ["https", "lambda", "sqs"])
q = (subs(OT, "sqs") or [{}])[0]
check("→ orders-events: raw delivery, the filter policy, its scope", q.get("target", {}).get("id") == f"sqs/{P}-orders-events" and q.get("rawDelivery")
      and json.loads(q.get("filterPolicy", "{}")) == {"type": ["order.created", "order.paid"]} and q.get("filterScope") == "MessageAttributes", str(q))
f = (subs(OT, "lambda") or [{}])[0]
check("→ audit-writer, its dead-letter queue orders-events-dlq", f.get("target", {}).get("id") == f"lambda/{P}-audit-writer"
      and (f.get("deadLetter") or {}).get("id") == f"sqs/{P}-orders-events-dlq", str(f))
h = (subs(OT, "https") or [{}])[0]
check("→ https: pending, no ARN", h.get("pending") and not h.get("arn"), str(h))
check("…its endpoint keeps the ALB's host", ".elb.amazonaws.com/sns/orders" in h.get("endpoint", ""), h.get("endpoint", "")[:30])
planted = json.load(open(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".work", "sns-planted.json")))
check("the planted password is nowhere in the inventory", planted["password"] not in text)
check("the planted query token is nowhere in the inventory", planted["token"] not in text)
check("the endpoint says what was withheld", "<redacted:url-credentials>" in h.get("endpoint", "") and "<redacted:url-query>" in h.get("endpoint", ""))
# --- the other two
check("alerts: its policy names CloudWatch, beside AWS's default statement",
      "cloudwatch.amazonaws.com" in json.dumps(spec(AL).get("policy")) and "AWS:SourceOwner" in json.dumps(spec(AL).get("policy")))
check("alerts → inbox", [s.get("target", {}).get("id") for s in subs(AL, "sqs")] == [f"sqs/{P}-inbox"])
fi = spec(FI)
check("notifications.fifo: FIFO, content-based deduplication", fi.get("fifo") and fi.get("contentBasedDeduplication"))
check("notifications.fifo → the FIFO queue of the same name, raw",
      [(s.get("target", {}).get("id"), s.get("rawDelivery")) for s in subs(FI, "sqs")] == [(f"sqs/{P}-notifications.fifo", True)])
check("every subscription owned by the scanned account", all(s.get("owner") == inv["accountId"] for t in [OT, AL, FI] for s in spec(t).get("subscriptions", [])))
check("nothing unread", not any(spec(t).get("unread") for t in [OT, AL, FI]))
# --- the receiving side
qp = json.dumps(spec(f"sqs/{P}-orders-events").get("policy"))
check("orders-events' policy: the role it had, and the topic", "orders-api-task" in qp and "sns.amazonaws.com" in qp and f"{P}-orders-topic" in qp)
check("inbox has no policy", not spec(f"sqs/{P}-inbox").get("policy"))
check("audit-writer's and webhook-receiver's policies name their topics",
      f"{P}-orders-topic" in json.dumps(spec(f"lambda/{P}-audit-writer").get("resourcePolicy"))
      and f"{P}-alerts" in json.dumps(spec(f"lambda/{P}-webhook-receiver").get("resourcePolicy")))
check("no SNS warnings", not [w for w in inv.get("warnings", []) if w.get("service") == "SNS"])

w = max(len(n) for _, n, _ in results); fails = 0
for ok, n, d in results:
    fails += not ok; print(f"  {'PASS' if ok else 'FAIL'}  {n:<{w}}  {'' if ok else d}")
print(f"\n{len(results)-fails}/{len(results)} checks passed"); sys.exit(1 if fails else 0)
