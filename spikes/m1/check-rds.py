#!/usr/bin/env python3
"""Asserts the RDS part of an inventory scanned from the M1 topology.

Written from what 14-aws-rds-create.sh builds — including what the free plan
forced on it (an express-configuration cluster: no VPC, no initial database) —
before the collector's output was looked at. Usage: check-rds.py <inventory.json>
"""
import json, sys
inv = json.load(open(sys.argv[1]))
P = "ce-test"
by_id = {r["id"]: r for r in inv["resources"]}
results = []
def check(name, ok, detail=""): results.append((bool(ok), name, detail))
spec = lambda i: by_id.get(i, {}).get("spec", {})

rds = sorted(i for i in by_id if i.startswith("rds/"))
check("the cluster and the instance are the RDS resources; the Aurora member is not one",
      rds == [f"rds/{P}-legacy-db", f"rds/cluster/{P}-db"], str(rds))
c, i = spec(f"rds/cluster/{P}-db"), spec(f"rds/{P}-legacy-db")
check("types", by_id.get(f"rds/cluster/{P}-db", {}).get("type") == "rds.cluster"
      and by_id.get(f"rds/{P}-legacy-db", {}).get("type") == "rds.instance")
# --- the cluster
check("cluster: aurora-postgresql, available", c.get("engine") == "aurora-postgresql" and c.get("status") == "available", f"{c.get('engine')} {c.get('status')}")
check("cluster: writer, reader endpoints under its account suffix",
      c.get("endpoint", "").startswith(f"{P}-db.cluster-") and c.get("readerEndpoint", "").startswith(f"{P}-db.cluster-ro-")
      and c["endpoint"].split(".", 1)[1][len("cluster-"):] == c["readerEndpoint"].split(".", 1)[1][len("cluster-ro-"):])
m = c.get("members", [])
check("cluster: one member, the writer express created, with its own endpoint",
      len(m) == 1 and m[0]["identifier"] == f"{P}-db-instance-1" and m[0].get("writer") and m[0].get("endpoint", "").startswith(f"{P}-db-instance-1."), str(m))
check("cluster: Serverless v2 at 0–1 ACU", (c.get("serverless") or {}).get("minAcu") == 0 and (c.get("serverless") or {}).get("maxAcu") == 1, str(c.get("serverless")))
check("cluster: Data API on (enabled after creation)", c.get("dataApi") is True)
check("cluster: express — outside any VPC, through an internet access gateway, no network recorded",
      c.get("internetAccessGateway") is True and not c.get("network"), str(c.get("network")))
check("cluster: no initial database, no managed secret (express refuses both)", not c.get("dbName") and not c.get("masterSecretArn"))
check("cluster: IAM authentication on, resource id recorded", c.get("iamAuth") is True and c.get("resourceId", "").startswith("cluster-"))
# --- the instance
check("instance: postgres db.t4g.micro, gp3 20 GB", i.get("engine") == "postgres" and i.get("instanceClass") == "db.t4g.micro"
      and i.get("storageType") == "gp3" and i.get("allocatedGb") == 20, f"{i.get('engine')} {i.get('instanceClass')}")
check("instance: endpoint and port", i.get("endpoint", "").startswith(f"{P}-legacy-db.") and i.get("port") == 5432)
check("instance: the managed master secret's ARN, never a value", ":secret:rds!db-" in i.get("masterSecretArn", ""))
check("instance: database, master user, IAM auth", i.get("dbName") == "legacy" and i.get("masterUsername") == "legacy_admin" and i.get("iamAuth") is True)
check("instance: in the default VPC, with subnets and a security group",
      (i.get("network") or {}).get("vpcId", "").startswith("vpc-") and len(i["network"].get("subnets", [])) >= 2 and i["network"].get("securityGroups"))
check("instance: not public, not a replica", not i.get("publiclyAccessible") and not i.get("replicaOf"))
check("tags came with the listing", by_id.get(f"rds/{P}-legacy-db", {}).get("tags", {}).get("cloud-echo-test") == "m1")
raw = json.dumps(inv)
check("no password anywhere", "not-a-real-password" not in raw)
check("no RDS warnings", not [w for w in inv.get("warnings", []) if w.get("service") == "RDS"], str([w for w in inv.get("warnings", []) if w.get("service") == "RDS"]))

w = max(len(n) for _, n, _ in results); fails = 0
for ok, n, d in results:
    fails += not ok; print(f"  {'PASS' if ok else 'FAIL'}  {n:<{w}}  {'' if ok else d}")
print(f"\n{len(results)-fails}/{len(results)} checks passed"); sys.exit(1 if fails else 0)
