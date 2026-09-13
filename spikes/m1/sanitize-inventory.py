#!/usr/bin/env python3
"""Turns a real inventory into a committable linker fixture.

Usage: sanitize-inventory.py <inventory.json> <out.json>

Drops Raw (the linker reads specs only), resources outside the ce-test- topology,
and every account-specific identifier: the account id, API / integration /
authorizer / deployment ids, mapping UUIDs, and VPC, subnet and security-group
ids. Each is replaced by a stable fake, so the graph keeps its shape. Refuses to
write if anything original survives.
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
text = json.dumps({**inv, "resources": keep, "scanId": "real-m1", "generatedBy": "sanitized from a real scan",
                   "scannedAt": "2026-09-13T00:00:00Z"}, indent=2)
for orig in re.findall(r"\b(?:subnet|sg|vpc)-[0-9a-f]{8,17}\b", text):
    fake(orig, orig.split("-")[0] + "-fake")
for orig, new in sorted(ids.items(), key=lambda kv: -len(kv[0])):
    text = text.replace(orig, new)
text = text.replace(acct, FAKE_ACCT)
out = json.loads(text)
out["resources"].sort(key=lambda r: r["id"])

leaks = [o for o in list(ids) + [acct] if o in json.dumps(out)]
if leaks:
    sys.exit(f"refusing to write: {len(leaks)} original identifier(s) survived")
json.dump(out, open(dst, "w"), indent=2); open(dst, "a").write("\n")
print(f"{len(out['resources'])} resources, {len(ids)} identifiers replaced, account id replaced")
