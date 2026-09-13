#!/usr/bin/env python3
"""Asserts the ELBv2 part of an inventory scanned from the M1 topology.

Written from what 16-aws-elbv2-create.sh builds, before the collector's output
was looked at. Usage: check-elbv2.py <inventory.json>
"""
import json, sys
inv = json.load(open(sys.argv[1]))
P = "ce-test"
by_id = {r["id"]: r for r in inv["resources"]}
results = []
def check(name, ok, detail=""): results.append((bool(ok), name, detail))
spec = lambda i: by_id.get(i, {}).get("spec", {})

ALB, NLB = f"elb/{P}-web", f"elb/{P}-tcp"
TGS = [f"elb/tg/{P}-fn-tg", f"elb/tg/{P}-tcp-tg", f"elb/tg/{P}-web-tg"]
check("two load balancers, three target groups", sorted(i for i in by_id if i.startswith("elb/")) == sorted([ALB, NLB] + TGS),
      str(sorted(i for i in by_id if i.startswith("elb/"))))
a, n = spec(ALB), spec(NLB)
# --- the ALB
check("ALB: application, internet-facing, active", (a.get("type"), a.get("scheme"), a.get("state")) == ("application", "internet-facing", "active"),
      f"{a.get('type')} {a.get('scheme')} {a.get('state')}")
check("ALB: DNS name in the ALB shape", a.get("dnsName", "").startswith(f"{P}-web-") and a["dnsName"].endswith(".us-east-2.elb.amazonaws.com"))
check("ALB: three subnets, a security group, its VPC", len(a.get("subnets", [])) == 3 and a.get("securityGroups") and a.get("vpcId", "").startswith("vpc-"))
ls = a.get("listeners", [])
rules = ls[0]["rules"] if len(ls) == 1 else []
check("ALB: one listener, HTTP:80", len(ls) == 1 and ls[0]["port"] == 80 and ls[0]["protocol"] == "HTTP")
check("ALB: rules in order — 10, 20, 30, then the default", [r["priority"] for r in rules] == ["10", "20", "30", "default"],
      str([r["priority"] for r in rules]))
if len(rules) == 4:
    check("rule 10: /fn/* → the Lambda target group", rules[0]["conditions"][0]["values"] == ["/fn/*"]
          and rules[0]["actions"][0]["targetGroups"][0]["arn"].split("/")[1] == f"{P}-fn-tg")
    check("rule 20: /old/* → a redirect", rules[1]["actions"][0]["type"] == "redirect" and rules[1]["actions"][0].get("redirect"))
    check("rule 30: /health → fixed response 200, body withheld", rules[2]["actions"][0]["type"] == "fixed-response"
          and rules[2]["actions"][0].get("fixedStatus") == "200")
    check("default → the ECS service's target group", rules[3]["actions"][0]["targetGroups"][0]["arn"].split("/")[1] == f"{P}-web-tg")
# Written as "MessageBody not in Raw"; the SDK type keeps the key, withheld as
# null — the check meant the value.
raw = json.dumps(by_id.get(ALB, {}).get("raw", {}))
check("no fixed-response body in Raw (withheld as null)", '"MessageBody": null' in raw and '"MessageBody": "' not in raw)
# --- the NLB
check("NLB: network, internal, active", (n.get("type"), n.get("scheme"), n.get("state")) == ("network", "internal", "active"))
check("NLB: DNS name in the NLB shape (.elb.<region>.amazonaws.com)", ".elb.us-east-2.amazonaws.com" in n.get("dnsName", ""), n.get("dnsName"))
nl = n.get("listeners", [])
check("NLB: TCP:6379, only a default forwarding to its target group", len(nl) == 1 and nl[0]["protocol"] == "TCP" and nl[0]["port"] == 6379
      and [r["priority"] for r in nl[0]["rules"]] == ["default"] and nl[0]["rules"][0]["actions"][0]["targetGroups"][0]["arn"].split("/")[1] == f"{P}-tcp-tg")
# --- target groups
fn, web, tcp = spec(TGS[0]), spec(TGS[2]), spec(TGS[1])
check("fn-tg: lambda, its one target is orders-fn", fn.get("targetType") == "lambda" and [t["id"] for t in fn.get("targets", [])] == [f"lambda/{P}-orders-fn"])
check("web-tg: ip, HTTP 8080, used by the ALB", web.get("targetType") == "ip" and web.get("protocol") == "HTTP" and web.get("port") == 8080
      and len(web.get("loadBalancerArns", [])) == 1 and not web.get("targets"))
check("tcp-tg: ip, TCP 6379, used by the NLB", tcp.get("targetType") == "ip" and tcp.get("protocol") == "TCP" and len(tcp.get("loadBalancerArns", [])) == 1)
check("tags, from DescribeTags", all(by_id.get(i, {}).get("tags", {}).get("cloud-echo-test") == "m1" for i in [ALB, NLB] + TGS))
# --- what points at them
svc = spec(f"ecs/{P}-batch/{P}-web")
check("ECS service ce-test-web registers in web-tg", [lb["targetGroupArn"].split("/")[1] for lb in svc.get("loadBalancers", [])] == [f"{P}-web-tg"])
http = next((r for r in inv["resources"] if r["type"] == "apigateway.http" and r["name"] == f"{P}-orders-http"), {})
vl = [rt for rt in http.get("spec", {}).get("routes", []) if rt["routeKey"] == "ANY /web/{proxy+}"]
check("the HTTP API's /web route goes through a VPC link to the ALB's listener", vl and vl[0]["integration"].get("connectionType") == "VPC_LINK"
      and ":listener/app/" + P + "-web/" in vl[0]["integration"].get("uri", ""), str(vl))
check("no ELB warnings", not [w for w in inv.get("warnings", []) if w.get("service") == "Elastic Load Balancing v2"])

w = max(len(n) for _, n, _ in results); fails = 0
for ok, n, d in results:
    fails += not ok; print(f"  {'PASS' if ok else 'FAIL'}  {n:<{w}}  {'' if ok else d}")
print(f"\n{len(results)-fails}/{len(results)} checks passed"); sys.exit(1 if fails else 0)
