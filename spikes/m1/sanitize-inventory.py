#!/usr/bin/env python3
"""Turns a real inventory into a committable linker fixture.

Usage: sanitize-inventory.py <inventory.json> <out.json>

Drops Raw (the linker reads specs only), resources outside the ce-test- topology,
and every account-specific identifier: the account id, API / integration /
authorizer / deployment / VPC link ids, mapping UUIDs, VPC, subnet and
security-group ids, RDS and ElastiCache endpoint suffixes, and load balancer ARN
ids and DNS hashes. Each is replaced by a stable fake, so the graph keeps its
shape. Refuses to write if anything original survives.
"""
import json, re, sys
src, dst = sys.argv[1], sys.argv[2]
inv = json.load(open(src))
acct = inv["accountId"]
FAKE_ACCT = "123456789012"

keep = [r for r in inv["resources"] if r["name"].startswith("ce-test") or r["id"].split("/", 1)[-1].startswith("ce-test")
        or r["type"] in ("lambda.event-source-mapping", "apigateway.rest", "apigateway.http", "apigateway.websocket")
        or (r["type"] == "iam.policy" and r["spec"].get("awsManaged"))]
ids = {}
def fake(orig, prefix):
    if orig not in ids:
        ids[orig] = f"{prefix}{sum(1 for v in ids.values() if v.startswith(prefix)) + 1:04d}"
    return ids[orig]

for r in keep:
    r.pop("raw", None)
    r["source"]["collectedAt"] = "2026-09-13T00:00:00Z"
    s = r["spec"]
    if r["type"].startswith("apigateway."):
        fake(s["apiId"], "fakeapi")
        for rt in s.get("routes") or []:
            it = rt.get("integration") or {}
            if it.get("id"): fake(it["id"], "int")
        for a in s.get("authorizers") or []: fake(a["id"], "auth")
        for st in s.get("stages") or []:
            if st.get("deploymentId"): fake(st["deploymentId"], "dep")
    if r["type"] == "lambda.event-source-mapping":
        fake(s["uuid"], "00000000-0000-4000-8000-00000000")
    if r["type"] in ("rds.cluster", "rds.instance"):
        # The endpoint suffix belongs to the account and region, and appears in
        # other resources' configuration too; resource ids and managed secret
        # names are account-specific as well.
        for e in [s.get("endpoint"), s.get("readerEndpoint")] + [m.get("endpoint") for m in s.get("members") or []]:
            m = re.search(r"\.(?:cluster-(?:ro-|custom-)?)?([a-z0-9]{12})\.[a-z0-9-]+\.rds\.amazonaws\.com$", e or "")
            if m:
                fake(m.group(1), "fakesfx")
        if s.get("resourceId"):
            kind, _, orig = s["resourceId"].partition("-")
            fake(orig, "FAKERESOURCEID")
        m = re.search(r":secret:(rds![a-z]+-[0-9a-f-]+-[A-Za-z0-9]{6})$", s.get("masterSecretArn", ""))
        if m:
            fake(m.group(1), "rds!db-fakesecret-")
    if r["type"].startswith("apigateway."):
        for rt in s.get("routes") or []:
            it = rt.get("integration") or {}
            if it.get("connectionType") == "VPC_LINK" and it.get("connectionId"): fake(it["connectionId"], "vlink")
    if r["type"] in ("elbv2.load-balancer", "elbv2.target-group"):
        # An NLB's DNS name reuses its ARN id; an ALB's carries a separate
        # decimal hash. Both reach other resources' configuration. (The ARN ids
        # are learned from the whole text below, wherever they appear: a
        # listener's is also in the API integration that names it.)
        m = re.fullmatch(r"(?:internal-)?" + re.escape(s.get("name", "")) + r"-([0-9a-z]+)\..*", s.get("dnsName", ""))
        if m:
            fake(m.group(1), "feedfacefeed" if len(m.group(1)) == 16 else "10000")
    if r["type"] == "elasticache.cache":
        # The account's suffix sits in a different place in each endpoint
        # shape: master.<rg>.<sfx>.use2…, <rg>-001.<rg>.<sfx>…, <id>.<sfx>.cfg…,
        # <name>-<sfx>.serverless… — found as the six-character label that is
        # neither a name nor a fixed word.
        known = {s["identifier"]} | {n["id"] for n in s.get("nodes") or []}
        fixed = {"master", "replica", "clustercfg", "cfg", "serverless", "ng", "cache", "amazonaws", "com"}
        for e in [s.get("primaryEndpoint"), s.get("readerEndpoint"), s.get("configurationEndpoint")] + [n.get("endpoint") for n in s.get("nodes") or []]:
            labels = (e or "").split(".")
            if len(labels) > 1 and labels[1] == "serverless":
                labels[0] = labels[0].rsplit("-", 1)[-1]
            for l in labels[:-3]:
                if re.fullmatch(r"[a-z0-9]{6}", l) and l not in known and l not in fixed and not l.isdigit() and not re.fullmatch(r"[a-z]{3}\d", l):
                    fake(l, "fk")
text = json.dumps({**inv, "resources": keep, "scanId": "real-m1", "generatedBy": "sanitized from a real scan",
                   "scannedAt": "2026-09-13T00:00:00Z"}, indent=2)
for orig in re.findall(r"\b(?:subnet|sg|vpc)-[0-9a-f]{8,17}\b", text):
    fake(orig, orig.split("-")[0] + "-fake")
# ELBv2 ARNs end in 16-hex ids — loadbalancer/<t>/<name>/<id>, targetgroup/<name>/<id>,
# listener/<t>/<name>/<lb-id>/<id> — faked as hex so the ARNs keep their shape.
for ids_ in re.findall(r"(?:loadbalancer|listener|listener-rule)/(?:app|net)/[a-z0-9-]+((?:/[0-9a-f]{16})+)|targetgroup/[a-z0-9-]+/([0-9a-f]{16})", text):
    for h in "".join(ids_).split("/"):
        if h: fake(h, "feedfacefeed")
for orig, new in sorted(ids.items(), key=lambda kv: -len(kv[0])):
    text = text.replace(orig, new)
text = text.replace(acct, FAKE_ACCT)
out = json.loads(text)
out["resources"].sort(key=lambda r: r["id"])

leaks = [o for o in list(ids) + [acct] if o in json.dumps(out)]
if leaks:
    sys.exit(f"refusing to write: {len(leaks)} original identifier(s) survived")
# What was never learned cannot be caught above: every ELB id and every
# load balancer DNS hash left must be a fake.
dump = json.dumps(out)
elb_left = [h for m in re.findall(r"(?:loadbalancer|listener|listener-rule)/(?:app|net)/[a-z0-9-]+((?:/[0-9a-f]+)+)|targetgroup/[a-z0-9-]+/([0-9a-f]+)", dump)
            for h in "".join(m).split("/") if h and not h.startswith("feedfacefeed")]
elb_left += [h for h in re.findall(r"-([0-9a-z]+)\.(?:[a-z0-9-]+\.elb|elb\.[a-z0-9-]+)\.amazonaws\.com", dump)
             if not (h.startswith(("feedfacefeed", "10000")) or set(h) == {"0"})]
if elb_left:
    sys.exit(f"refusing to write: {len(elb_left)} load balancer identifier(s) not faked")
json.dump(out, open(dst, "w"), indent=2); open(dst, "a").write("\n")
print(f"{len(out['resources'])} resources, {len(ids)} identifiers replaced, account id replaced")
