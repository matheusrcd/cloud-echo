#!/usr/bin/env python3
"""Asserts the ElastiCache part of an inventory scanned from the M1 topology.

Written from what 15-aws-elasticache-create.sh builds, before the collector's
output was looked at. Usage: check-elasticache.py <inventory.json>
"""
import json, sys
inv = json.load(open(sys.argv[1]))
P = "ce-test"
by_id = {r["id"]: r for r in inv["resources"]}
results = []
def check(name, ok, detail=""): results.append((bool(ok), name, detail))
spec = lambda i: by_id.get(i, {}).get("spec", {})

RG, SL, MC = f"cache/{P}-sessions", f"cache/serverless/{P}-ratelimit", f"cache/cluster/{P}-memo"
caches = sorted(i for i in by_id if i.startswith("cache/"))
check("one cache per API; the replication group's member is not a cache", caches == sorted([RG, SL, MC]), str(caches))
check("all three are elasticache.cache", all(by_id.get(i, {}).get("type") == "elasticache.cache" for i in (RG, SL, MC)))
# --- replication group
g = spec(RG)
check("group: valkey, available, no cluster mode", g.get("kind") == "replication-group" and g.get("engine") == "valkey"
      and g.get("status") == "available" and not g.get("clusterMode"), f"{g.get('engine')} {g.get('status')}")
check("group: engine version, from its member", g.get("engineVersion", "").startswith("9."), g.get("engineVersion"))
check("group: primary and reader endpoints, port 6379", g.get("primaryEndpoint", "").endswith(".cache.amazonaws.com")
      and g.get("readerEndpoint", "").endswith(".cache.amazonaws.com") and g["primaryEndpoint"] != g["readerEndpoint"] and g.get("port") == 6379)
n = g.get("nodes", [])
check("group: one node, the primary, with its own endpoint", len(n) == 1 and n[0]["id"] == f"{P}-sessions-001"
      and n[0].get("role") == "primary" and n[0].get("endpoint"), str(n))
check("group: TLS required, no AUTH token", g.get("transitEncryption") is True and not g.get("authToken"))
# Created without --security-group-ids, a cache cluster runs under the VPC's
# default group — and DescribeCacheClusters lists none (SecurityGroups: null).
# The first run expected one "from its member"; the account said otherwise.
# A serverless cache, by contrast, lists the default group explicitly.
check("group: its subnet group, from its member; no security group listed (the VPC default applies, unlisted)",
      (g.get("network") or {}).get("subnetGroup") == f"{P}-cache-subnets" and not (g.get("network") or {}).get("securityGroups"),
      str(g.get("network")))
# --- serverless
s = spec(SL)
check("serverless: valkey, available", s.get("kind") == "serverless" and s.get("engine") == "valkey" and s.get("status") == "available")
check("serverless: reader is the same host (another port)", s.get("primaryEndpoint") and s.get("primaryEndpoint") == s.get("readerEndpoint")
      and s.get("port") == 6379)
check("serverless: the caps the script set", (s.get("limits") or {}).get("maxStorageGb") == 1 and (s.get("limits") or {}).get("maxEcpuPerSecond") == 1000)
check("serverless: subnets and a security group, TLS", len((s.get("network") or {}).get("subnets", [])) == 3
      and (s.get("network") or {}).get("securityGroups") and s.get("transitEncryption") is True)
# --- memcached
m = spec(MC)
check("memcached: 1.6, available", m.get("kind") == "cache-cluster" and m.get("engine") == "memcached"
      and m.get("engineVersion", "").startswith("1.6") and m.get("status") == "available")
check("memcached: configuration endpoint on 11211, one node", ".cfg." in m.get("configurationEndpoint", "") and m.get("port") == 11211
      and len(m.get("nodes", [])) == 1 and m["nodes"][0].get("endpoint"))
# --- common
check("tags, from ListTagsForResource", all(by_id.get(i, {}).get("tags", {}).get("cloud-echo-test") == "m1" for i in (RG, SL, MC)))
check("no ElastiCache warnings", not [w for w in inv.get("warnings", []) if w.get("service") == "ElastiCache"],
      str([w for w in inv.get("warnings", []) if w.get("service") == "ElastiCache"]))

w = max(len(n) for _, n, _ in results); fails = 0
for ok, n, d in results:
    fails += not ok; print(f"  {'PASS' if ok else 'FAIL'}  {n:<{w}}  {'' if ok else d}")
print(f"\n{len(results)-fails}/{len(results)} checks passed"); sys.exit(1 if fails else 0)
