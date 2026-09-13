#!/usr/bin/env python3
"""Groups diff-inventory output by resource type and field path, so a fidelity
gap repeated across 13 ECS services reads as one line with a count."""
import json, re, subprocess, sys
from collections import defaultdict
here = sys.path[0]
a, b = sys.argv[1], sys.argv[2]
out = subprocess.run([sys.executable, f"{here}/diff-inventory.py", a, b, "--ignore-raw", "--all"], capture_output=True, text=True).stdout
types = {}
for p in (a, b):
    for r in json.load(open(p))["resources"]:
        types[r["id"]] = r["type"]
groups = defaultdict(list)
lines = out.splitlines()
i = 0
while i < len(lines):
    m = re.match(r"^  (\S.*)$", lines[i])
    if m and i + 2 < len(lines) and lines[i+1].strip().startswith("a:"):
        path = m.group(1)
        va, vb = lines[i+1].split("a:", 1)[1].strip(), lines[i+2].split("b:", 1)[1].strip()
        # ids can contain dots (sqs/x.fifo): take the longest known id prefix
        rid = max((k for k in types if path == k or path.startswith(k + ".")), key=len, default=path.split(".", 1)[0])
        field = path[len(rid) + 1:] if len(path) > len(rid) else "<resource>"
        field = re.sub(r"\[\d+\]", "[]", field)
        t = types.get(rid, "lambda.event-source-mapping" if rid.startswith("lambda/esm[") else "?")
        groups[(t, field)].append((rid, va, vb))
        i += 3
    else:
        i += 1
for (t, field), items in sorted(groups.items()):
    rid, va, vb = items[0]
    print(f"{t:<28} {field:<44} ×{len(items):<3} aws={va[:52]!s:<54} floci={vb[:52]}")
print(f"\n{sum(len(v) for v in groups.values())} differences in {len(groups)} distinct (type, field) groups")
