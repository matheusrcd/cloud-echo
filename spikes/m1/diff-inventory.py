#!/usr/bin/env python3
"""Compares two inventories resource by resource, ignoring fields that are
expected to differ between scans (scan id, timestamps, generator).

Usage: diff-inventory.py a.json b.json [--ignore-raw]
Exit 0 when equivalent. Prints every differing path, so a difference is a
finding with an address rather than "the files differ".
"""
import json, sys

VOLATILE_TOP = {"scanId", "scannedAt", "generatedBy"}
args = [a for a in sys.argv[1:] if not a.startswith("--")]
ignore_raw = "--ignore-raw" in sys.argv

def load(p):
    inv = json.load(open(p))
    res = {}
    # API ids are minted per environment; pair APIs by type and name, and
    # rewrite references to them (IAM assumedBy) the same way.
    api_name = {r["id"]: f"apigw[{r['type']}:{r['name']}]" for r in inv["resources"] if r["type"].startswith("apigateway.")}
    for r in inv["resources"]:
        r = dict(r)
        r.get("source", {}).pop("collectedAt", None)
        if ignore_raw:
            r.pop("raw", None)
        if r["type"] == "ecs.service" and isinstance(r.get("raw"), dict):
            # The service's event log is a moving window ECS appends to on its
            # own ("reached a steady state"); two scans a minute apart can
            # straddle an entry. Found in the ELBv2 round: 24 differences, all
            # here, none from permissions.
            r["raw"] = {k: v for k, v in r["raw"].items() if k != "Events"}
        key = r["id"]
        if r["type"] == "lambda.event-source-mapping":
            # Mapping ids are UUIDs, minted per environment and per re-creation.
            # Pair them by what they connect instead; the UUID-bearing fields
            # cannot match by construction and are dropped.
            s = r["spec"]
            key = f"lambda/esm[{s['source'].get('id') or s['source']['arn']} -> {s.get('functionId')}{':' + s['qualifier'] if s.get('qualifier') else ''}]"
            for k in ("id", "arn", "name"):
                r.pop(k, None)
            s.pop("uuid", None)
            if s.get("sourceType") == "dynamodb-stream":
                s["source"].pop("arn", None)  # stream label is a creation timestamp
        if r["type"].startswith("apigateway."):
            key = api_name[r["id"]]
            for k in ("id", "arn"):
                r.pop(k, None)
            s = r["spec"]
            for k in ("apiId", "endpoint"):
                s.pop(k, None)
            for rt in s.get("routes") or []:
                rt.pop("authorizerId", None)
                (rt.get("integration") or {}).pop("id", None)
            for a in s.get("authorizers") or []:
                a.pop("id", None)
            for st in s.get("stages") or []:
                st.pop("deploymentId", None)
        if r["type"] == "iam.role":
            r["spec"]["assumedBy"] = sorted(api_name.get(x, x) for x in r["spec"].get("assumedBy") or [])
        res[key] = r
    top = {k: v for k, v in inv.items() if k not in VOLATILE_TOP and k != "resources"}
    return top, res

def walk(a, b, path, out):
    if type(a) != type(b):
        out.append((path, a, b)); return
    if isinstance(a, dict):
        for k in sorted(set(a) | set(b)):
            walk(a.get(k, "<absent>"), b.get(k, "<absent>"), f"{path}.{k}", out)
    elif isinstance(a, list):
        if len(a) != len(b):
            out.append((path + "[len]", len(a), len(b)))
        for i, (x, y) in enumerate(zip(a, b)):
            walk(x, y, f"{path}[{i}]", out)
    elif a != b:
        out.append((path, a, b))

ta, ra = load(args[0]); tb, rb = load(args[1])
diffs = []
walk(ta, tb, "inventory", diffs)
for rid in sorted(set(ra) | set(rb)):
    if rid not in ra or rid not in rb:
        diffs.append((rid, "present" if rid in ra else "<absent>", "present" if rid in rb else "<absent>")); continue
    walk(ra[rid], rb[rid], rid, diffs)

limit = None if "--all" in sys.argv else 40
for path, x, y in diffs[:limit]:
    print(f"  {path}\n      a: {str(x)[:110]}\n      b: {str(y)[:110]}")
if limit and len(diffs) > limit:
    print(f"  ... {len(diffs) - 40} more")
print(f"\n{len(diffs)} difference(s) across {len(set(ra) | set(rb))} resources")
sys.exit(1 if diffs else 0)
