#!/usr/bin/env python3
"""Prototype seeder: rebuild a scanned inventory inside a local Floci.

This is the M2 materializer's question asked early: does the inventory hold
enough to recreate the account? It builds from the normalized *spec* — what the
planner will consume — and falls back to Raw only where the spec lacks
something. Every fallback is reported as a spec gap, every failed call as an
emulation gap. Those two lists are the point of running it.

Refuses to write anywhere but localhost, and strips every AWS_* variable from
the child environment so no profile or real credential can be picked up.
"""
import json, os, subprocess, sys, tempfile, urllib.parse, zipfile

ENDPOINT = os.environ.get("FLOCI_ENDPOINT", "http://localhost:4566")
if urllib.parse.urlparse(ENDPOINT).hostname not in ("localhost", "127.0.0.1"):
    sys.exit(f"refusing to seed {ENDPOINT}: the seeder only ever writes to a local endpoint")

inv = json.load(open(sys.argv[1]))
ACCT, REGION = inv["accountId"], inv["region"]
ENV = {k: v for k, v in os.environ.items() if not k.startswith("AWS_")}
ENV.update(AWS_ACCESS_KEY_ID=ACCT, AWS_SECRET_ACCESS_KEY="test", AWS_REGION=REGION, AWS_PAGER="",
           AWS_CONFIG_FILE="/dev/null", AWS_SHARED_CREDENTIALS_FILE="/dev/null")

# Fail fast: 44 identical "could not connect" failures say less than one line.
probe = subprocess.run(["aws", "--endpoint-url", ENDPOINT, "sts", "get-caller-identity", "--query", "Account", "--output", "text"],
                       env=ENV, capture_output=True, text=True)
if probe.returncode != 0 or probe.stdout.strip() != ACCT:
    sys.exit(f"{ENDPOINT} is not a Floci answering as the scanned account: {(probe.stderr or probe.stdout).strip()[:160]}")

res = [r for r in inv["resources"]]
of = lambda t: sorted((r for r in res if r["type"] == t), key=lambda r: r["id"])
ok, failed, gaps = [], [], []

def aws(rid, step, *args):
    p = subprocess.run(["aws", "--endpoint-url", ENDPOINT, "--output", "json", *args],
                       env=ENV, capture_output=True, text=True)
    if p.returncode == 0:
        ok.append((rid, step))
        return json.loads(p.stdout) if p.stdout.strip() else {}
    msg = (p.stderr.strip().splitlines() or ["?"])[-1].replace(ACCT, "<ACCT>")
    failed.append((rid, step, msg[:170]))
    return None

def gap(rid, what):
    gaps.append((rid, what))

# ---------------------------------------------------------------- DynamoDB
for r in of("dynamodb.table"):
    s, name = r["spec"], r["name"]
    attrs = {}
    for ks in [s["keySchema"]] + [g["keySchema"] for g in (s.get("globalSecondaryIndexes") or []) + (s.get("localSecondaryIndexes") or [])]:
        for k in ks:
            attrs[k["name"]] = k["type"]
    args = ["dynamodb", "create-table", "--table-name", name,
            "--attribute-definitions", json.dumps([{"AttributeName": k, "AttributeType": v} for k, v in attrs.items()]),
            "--key-schema", json.dumps([{"AttributeName": k["name"], "KeyType": k["role"]} for k in s["keySchema"]])]
    pt = None
    if s.get("billingMode") == "PROVISIONED":
        tp = s.get("provisionedThroughput")
        if not tp:
            gap(r["id"], "provisioned table without provisionedThroughput in the spec")
            tp = {"read": 1, "write": 1}
        pt = {"ReadCapacityUnits": tp["read"], "WriteCapacityUnits": tp["write"]}
        args += ["--billing-mode", "PROVISIONED", "--provisioned-throughput", json.dumps(pt)]
    else:
        args += ["--billing-mode", "PAY_PER_REQUEST"]
    gsis = []
    for g in s.get("globalSecondaryIndexes") or []:
        proj = {"ProjectionType": g.get("projection") or "ALL"}
        if g.get("nonKeyAttributes"):
            proj["NonKeyAttributes"] = g["nonKeyAttributes"]
        gi = {"IndexName": g["name"], "KeySchema": [{"AttributeName": k["name"], "KeyType": k["role"]} for k in g["keySchema"]], "Projection": proj}
        if pt:
            gtp = g.get("provisionedThroughput") or {"read": pt["ReadCapacityUnits"], "write": pt["WriteCapacityUnits"]}
            gi["ProvisionedThroughput"] = {"ReadCapacityUnits": gtp["read"], "WriteCapacityUnits": gtp["write"]}
        gsis.append(gi)
    if gsis:
        args += ["--global-secondary-indexes", json.dumps(gsis)]
    if s.get("stream"):
        args += ["--stream-specification", f"StreamEnabled=true,StreamViewType={s['stream']['viewType']}"]
    if aws(r["id"], "create-table", *args) is not None and s.get("ttl"):
        aws(r["id"], "update-time-to-live", "dynamodb", "update-time-to-live", "--table-name", name,
            "--time-to-live-specification", f"Enabled=true,AttributeName={s['ttl']['attribute']}")

# ---------------------------------------------------------------- SQS
for r in of("sqs.queue"):
    s = r["spec"]
    a = {"VisibilityTimeout": str(s.get("visibilityTimeout", 30))}
    for k, sk in [("MessageRetentionPeriod", "messageRetentionPeriod"), ("DelaySeconds", "delaySeconds"), ("MaximumMessageSize", "maximumMessageSize")]:
        if s.get(sk):
            a[k] = str(s[sk])
    if s.get("fifo"):
        a["FifoQueue"] = "true"
        a["ContentBasedDeduplication"] = "true" if s.get("contentBasedDeduplication") else "false"
    aws(r["id"], "create-queue", "sqs", "create-queue", "--queue-name", s["queueName"], "--attributes", json.dumps(a))
for r in of("sqs.queue"):  # second pass: redrive targets must exist first
    s, a = r["spec"], {}
    if s.get("redrive"):
        a["RedrivePolicy"] = json.dumps({"deadLetterTargetArn": s["redrive"]["deadLetterTargetArn"], "maxReceiveCount": s["redrive"]["maxReceiveCount"]})
    if s.get("policy"):
        a["Policy"] = json.dumps(s["policy"])
    if a:
        aws(r["id"], "set-queue-attributes", "sqs", "set-queue-attributes", "--queue-url", s["url"], "--attributes", json.dumps(a))

# ---------------------------------------------------------------- IAM
aws_managed_missing = set()
for r in of("iam.policy"):
    s = r["spec"]
    if s["awsManaged"]:
        if aws(r["id"], "get-policy (AWS-managed present?)", "iam", "get-policy", "--policy-arn", r["arn"]) is None:
            aws_managed_missing.add(r["arn"])
        continue
    aws(r["id"], "create-policy", "iam", "create-policy", "--policy-name", s["policyName"], "--path", s.get("path") or "/",
        "--policy-document", json.dumps(s["document"]))
for r in of("iam.role"):
    s = r["spec"]
    args = ["iam", "create-role", "--role-name", s["roleName"], "--path", s.get("path") or "/",
            "--assume-role-policy-document", json.dumps(s["trustPolicy"])]
    if s.get("permissionsBoundary"):
        args += ["--permissions-boundary", s["permissionsBoundary"]["arn"]]
    if aws(r["id"], "create-role", *args) is None:
        continue
    for ip in s.get("inlinePolicies") or []:
        aws(r["id"], f"put-role-policy {ip['name']}", "iam", "put-role-policy", "--role-name", s["roleName"],
            "--policy-name", ip["name"], "--policy-document", json.dumps(ip["document"]))
    for ap in s.get("attachedPolicies") or []:
        aws(r["id"], f"attach-role-policy {ap['id']}", "iam", "attach-role-policy", "--role-name", s["roleName"], "--policy-arn", ap["arn"])

# ---------------------------------------------------------------- Lambda
tmp = tempfile.mkdtemp()
def stub_zip(runtime, handler):
    mod, fn = (handler or "index.handler").rsplit(".", 1)
    path = os.path.join(tmp, f"{mod}-{fn}-{runtime}.zip")
    with zipfile.ZipFile(path, "w") as z:
        if runtime.startswith("python"):
            z.writestr(f"{mod}.py", f"def {fn}(event, context):\n    return {{'stub': True}}\n")
        else:
            z.writestr(f"{mod}.js", f"exports.{fn} = async () => ({{stub: true}});\n")
    return path

qualifiers = {}
for r in of("lambda.event-source-mapping"):
    if r["spec"].get("qualifier") and r["spec"].get("functionId"):
        qualifiers.setdefault(r["spec"]["functionId"], set()).add(r["spec"]["qualifier"])

for r in of("lambda.function"):
    s, name = r["spec"], r["name"]
    if s.get("redacted"):
        gap(r["id"], f"{len(s['redacted'])} redacted value(s) seeded as markers — the planner must substitute local values")
    args = ["lambda", "create-function", "--function-name", name, "--runtime", s["runtime"], "--handler", s["handler"],
            "--role", s["roleArn"], "--zip-file", f"fileb://{stub_zip(s['runtime'], s['handler'])}",
            "--timeout", str(s["timeoutSec"]), "--memory-size", str(s["memoryMb"])]
    if s.get("architectures"):
        args += ["--architectures", *s["architectures"]]
    if s.get("env"):
        args += ["--environment", json.dumps({"Variables": s["env"]})]
    if s.get("deadLetter"):
        args += ["--dead-letter-config", f"TargetArn={s['deadLetter']['arn']}"]
    if s.get("envUnreadable"):
        gap(r["id"], "environment was unreadable at scan time; recreated without it")
    if aws(r["id"], "create-function", *args) is None:
        continue
    for st in (s.get("resourcePolicy") or {}).get("Statement", []):
        principal = st.get("Principal", {}).get("Service") or st.get("Principal", {}).get("AWS")
        src = ((st.get("Condition") or {}).get("ArnLike") or {}).get("AWS:SourceArn")
        args = ["lambda", "add-permission", "--function-name", name, "--statement-id", st.get("Sid", "stmt"),
                "--action", st["Action"], "--principal", principal]
        if src:
            args += ["--source-arn", src]
        aws(r["id"], "add-permission", *args)
    for q in sorted(qualifiers.get(r["id"], [])):
        gap(r["id"], f"alias '{q}' inferred from a mapping's qualifier — aliases and versions are not in the inventory")
        v = aws(r["id"], "publish-version", "lambda", "publish-version", "--function-name", name)
        if v:
            aws(r["id"], f"create-alias {q}", "lambda", "create-alias", "--function-name", name, "--name", q, "--function-version", v["Version"])

for r in of("lambda.event-source-mapping"):
    s = r["spec"]
    fn = s["functionId"].split("/", 1)[1] + (f":{s['qualifier']}" if s.get("qualifier") else "")
    src = s["source"]["arn"]
    if s["sourceType"] == "dynamodb-stream":
        d = aws(r["id"], "describe-table (local stream arn)", "dynamodb", "describe-table", "--table-name", s["source"]["id"].split("/", 1)[1])
        if not d:
            continue
        gap(r["id"], "DynamoDB stream ARNs carry a creation timestamp and differ per environment; the source must be re-resolved, not copied")
        src = d["Table"]["LatestStreamArn"]
    args = ["lambda", "create-event-source-mapping", "--function-name", fn, "--event-source-arn", src]
    if s.get("batchSize"):
        args += ["--batch-size", str(s["batchSize"])]
    if s.get("maxBatchingWindowSec"):
        args += ["--maximum-batching-window-in-seconds", str(s["maxBatchingWindowSec"])]
    if s.get("startingPosition"):
        args += ["--starting-position", s["startingPosition"]]
    if s.get("filters"):
        args += ["--filter-criteria", json.dumps({"Filters": [{"Pattern": p} for p in s["filters"]]})]
    if s.get("onFailure"):
        args += ["--destination-config", json.dumps({"OnFailure": {"Destination": s["onFailure"]["arn"]}})]
    args += ["--enabled"] if s.get("enabled") is not False else ["--no-enabled"]
    aws(r["id"], "create-event-source-mapping", *args)

# ---------------------------------------------------------------- ECS
for r in of("ecs.cluster"):
    aws(r["id"], "create-cluster", "ecs", "create-cluster", "--cluster-name", r["name"])

def container_def(c, raw_c):
    d = {"name": c["name"], "image": c["image"], "essential": c["essential"]}
    for k in ("command", "entryPoint"):
        if c.get(k):
            d[k] = c[k]
    if c.get("env"):
        d["environment"] = [{"name": k, "value": v} for k, v in sorted(c["env"].items())]
    if c.get("secrets"):
        d["secrets"] = [{"name": k, "valueFrom": v} for k, v in sorted(c["secrets"].items())]
    if c.get("portMappings"):
        d["portMappings"] = [{k2: v2 for k2, v2 in {"containerPort": p["containerPort"], "protocol": p.get("protocol"), "name": p.get("name")}.items() if v2} for p in c["portMappings"]]
    if c.get("dependsOn"):
        d["dependsOn"] = [{"containerName": x["container"], "condition": x["condition"]} for x in c["dependsOn"]]
    if c.get("log"):
        lc = {"logDriver": c["log"]["driver"]}
        if c["log"].get("options"):
            lc["options"] = c["log"]["options"]
        if c["log"].get("secretOptions"):
            lc["secretOptions"] = [{"name": k, "valueFrom": v} for k, v in sorted(c["log"]["secretOptions"].items())]
        d["logConfiguration"] = lc
    return d

families = {}
for r in of("ecs.taskdefinition"):
    families.setdefault(r["spec"]["family"], {})[r["spec"]["revision"]] = r
for fam, revs in sorted(families.items()):
    top = max(revs)
    for rev in range(1, top + 1):
        # Floci numbers revisions from 1. Registering a filler for each revision
        # the inventory lacks keeps the ids comparable — and shows that revision
        # numbers cannot be reproduced by registering once.
        r = revs.get(rev) or revs[min(k for k in revs if k > rev)]
        if rev not in revs:
            gap(f"ecs/taskdef/{fam}:{rev}", "revision absent from the inventory (unused); registered a filler to keep numbering aligned")
        s, raw_cs = r["spec"], {c.get("Name"): c for c in (r.get("raw") or {}).get("ContainerDefinitions") or []}
        if any(c.get("redacted") for c in s["containers"]):
            gap(r["id"], "redacted values seeded as markers — the planner must substitute local values")
        td = {"family": fam, "containerDefinitions": [container_def(c, raw_cs.get(c["name"], {})) for c in s["containers"]]}
        for k, sk in [("networkMode", "networkMode"), ("cpu", "cpu"), ("memory", "memory"), ("taskRoleArn", "taskRoleArn"), ("executionRoleArn", "executionRoleArn")]:
            if s.get(sk):
                td[k] = s[sk]
        if s.get("requiresCompatibilities"):
            td["requiresCompatibilities"] = s["requiresCompatibilities"]
        f = os.path.join(tmp, f"td-{fam}-{rev}.json")
        json.dump(td, open(f, "w"))
        aws(f"ecs/taskdef/{fam}:{rev}", "register-task-definition", "ecs", "register-task-definition", "--cli-input-json", f"file://{f}")

for r in of("ecs.service"):
    s = r["spec"]
    args = ["ecs", "create-service", "--cluster", s["cluster"], "--service-name", s["serviceName"],
            "--task-definition", s["taskDefinitionId"].split("/", 2)[2], "--desired-count", str(s["desiredCount"])]
    if s.get("launchType"):
        args += ["--launch-type", s["launchType"]]
    if s.get("network"):
        n = s["network"]
        args += ["--network-configuration", json.dumps({"awsvpcConfiguration": {"subnets": n.get("subnets") or [], "securityGroups": n.get("securityGroups") or [], "assignPublicIp": n.get("assignPublicIp") or "DISABLED"}})]
    aws(r["id"], "create-service", *args)

# ---------------------------------------------------------------- API Gateway
def invoke_uri(fn_arn):
    return f"arn:aws:apigateway:{REGION}:lambda:path/2015-03-31/functions/{fn_arn}/invocations"

def seed_param(v):
    # REST APIs quote literals. A redaction marker was a quoted literal before it
    # was withheld, so it is re-quoted to stay a valid mapping.
    return f"'{v}'" if v.startswith("<redacted:") else v

for r in of("apigateway.rest"):
    s = r["spec"]
    args = ["apigateway", "create-rest-api", "--name", s["name"]]
    if s.get("endpointType"):
        args += ["--endpoint-configuration", f"types={s['endpointType']}"]
    if s.get("resourcePolicy"):
        args += ["--policy", json.dumps(s["resourcePolicy"])]
    api = aws(r["id"], "create-rest-api", *args)
    if not api:
        continue
    aid = api["id"]
    root = aws(r["id"], "get-resources (root)", "apigateway", "get-resources", "--rest-api-id", aid)
    ids = {"/": next(x["id"] for x in root["items"] if x["path"] == "/")}
    auth_map = {}
    for a in s.get("authorizers") or []:
        aargs = ["apigateway", "create-authorizer", "--rest-api-id", aid, "--name", a["name"], "--type", a["type"]]
        if a.get("function"):
            aargs += ["--authorizer-uri", invoke_uri(a["function"]["arn"])]
            gap(r["id"], "authorizer invoke URI rebuilt from the function ARN — the spec keeps the function, not the URI")
        if a.get("identitySource"):
            aargs += ["--identity-source", ",".join(a["identitySource"])]
        na = aws(r["id"], f"create-authorizer {a['name']}", *aargs)
        if na:
            auth_map[a["id"]] = na["id"]
    for rt in sorted(s["routes"], key=lambda x: x["path"].count("/")):
        path = rt["path"]
        parent = "/"
        for seg in [p for p in path.split("/") if p]:
            cur = (parent.rstrip("/") + "/" + seg)
            if cur not in ids:
                nr = aws(r["id"], f"create-resource {cur}", "apigateway", "create-resource", "--rest-api-id", aid,
                         "--parent-id", ids[parent], "--path-part", seg)
                if not nr:
                    break
                ids[cur] = nr["id"]
            parent = cur
        rid = ids.get(path)
        if not rid:
            continue
        margs = ["apigateway", "put-method", "--rest-api-id", aid, "--resource-id", rid, "--http-method", rt["method"],
                 "--authorization-type", rt.get("authorization") or "NONE"]
        if rt.get("authorizerId") in auth_map:
            margs += ["--authorizer-id", auth_map[rt["authorizerId"]]]
        if rt.get("apiKeyRequired"):
            margs += ["--api-key-required"]
        if aws(r["id"], f"put-method {rt['routeKey']}", *margs) is None:
            continue
        it = rt.get("integration")
        if not it:
            continue
        iargs = ["apigateway", "put-integration", "--rest-api-id", aid, "--resource-id", rid, "--http-method", rt["method"], "--type", it["type"]]
        if it.get("uri"):
            iargs += ["--uri", it["uri"]]
        if it.get("httpMethod"):
            iargs += ["--integration-http-method", it["httpMethod"]]
        if it.get("credentials"):
            iargs += ["--credentials", it["credentials"]]
        if it.get("requestParameters"):
            iargs += ["--request-parameters", json.dumps({k: seed_param(v) for k, v in it["requestParameters"].items()})]
        if it.get("templateContentTypes"):
            gap(r["id"], "mapping templates are withheld by design; a placeholder template is seeded")
            iargs += ["--request-templates", json.dumps({ct: "{}" for ct in it["templateContentTypes"]})]
        aws(r["id"], f"put-integration {rt['routeKey']}", *iargs)
    for st in s.get("stages") or []:
        dargs = ["apigateway", "create-deployment", "--rest-api-id", aid, "--stage-name", st["name"]]
        if st.get("variables"):
            dargs += ["--variables", ",".join(f"{k}={v}" for k, v in sorted(st["variables"].items()))]
        aws(r["id"], f"create-deployment {st['name']}", *dargs)

for r in of("apigateway.http"):
    s = r["spec"]
    api = aws(r["id"], "create-api", "apigatewayv2", "create-api", "--name", s["name"], "--protocol-type", s["protocol"])
    if not api:
        continue
    aid = api["ApiId"]
    int_map, auth_map = {}, {}
    for rt in s["routes"]:
        it = rt.get("integration")
        if not it or it.get("id") in int_map:
            continue
        iargs = ["apigatewayv2", "create-integration", "--api-id", aid, "--integration-type", it["type"]]
        for flag, key in [("--integration-uri", "uri"), ("--integration-subtype", "subtype"), ("--integration-method", "httpMethod"),
                          ("--credentials-arn", "credentials"), ("--payload-format-version", "payloadFormatVersion")]:
            if it.get(key):
                iargs += [flag, it[key]]
        if it.get("requestParameters"):
            iargs += ["--request-parameters", json.dumps(it["requestParameters"])]
        ni = aws(r["id"], f"create-integration {it.get('id')}", *iargs)
        if ni:
            int_map[it["id"]] = ni["IntegrationId"]
    for a in s.get("authorizers") or []:
        aargs = ["apigatewayv2", "create-authorizer", "--api-id", aid, "--name", a["name"], "--authorizer-type", a["type"],
                 "--identity-source", *(a.get("identitySource") or [])]
        if a.get("jwtIssuer"):
            aargs += ["--jwt-configuration", json.dumps({"Issuer": a["jwtIssuer"], "Audience": a.get("jwtAudience") or []})]
        na = aws(r["id"], f"create-authorizer {a['name']}", *aargs)
        if na:
            auth_map[a["id"]] = na["AuthorizerId"]
    for rt in s["routes"]:
        rargs = ["apigatewayv2", "create-route", "--api-id", aid, "--route-key", rt["routeKey"]]
        if rt.get("integration") and rt["integration"].get("id") in int_map:
            rargs += ["--target", "integrations/" + int_map[rt["integration"]["id"]]]
        if rt.get("authorization") and rt["authorization"] != "NONE":
            rargs += ["--authorization-type", rt["authorization"]]
            if rt.get("authorizerId") in auth_map:
                rargs += ["--authorizer-id", auth_map[rt["authorizerId"]]]
        aws(r["id"], f"create-route {rt['routeKey']}", *rargs)
    for st in s.get("stages") or []:
        sargs = ["apigatewayv2", "create-stage", "--api-id", aid, "--stage-name", st["name"]]
        if st.get("autoDeploy"):
            sargs += ["--auto-deploy"]
        if st.get("variables"):
            sargs += ["--stage-variables", ",".join(f"{k}={v}" for k, v in sorted(st["variables"].items()))]
        aws(r["id"], f"create-stage {st['name']}", *sargs)

# ---------------------------------------------------------------- RDS
# Built from the spec alone. The master password is never in the inventory —
# RDS does not return it, and cloud-echo would not keep it — so a local one is
# made up here, the way the materializer will (docs/07-security.md).
LOCAL_PW = "local-only-not-a-secret"
def db_auth(s):
    user = s.get("masterUsername") or "postgres"
    if s.get("masterSecretArn"):
        return ["--master-username", user, "--manage-master-user-password"]
    return ["--master-username", user, "--master-user-password", LOCAL_PW]

for r in of("rds.cluster"):
    s = r["spec"]
    args = ["rds", "create-db-cluster", "--db-cluster-identifier", s["identifier"], "--engine", s["engine"],
            "--engine-version", s["engineVersion"], *db_auth(s)]
    if s.get("dbName"):
        args += ["--database-name", s["dbName"]]
    if sv := s.get("serverless"):
        args += ["--serverless-v2-scaling-configuration", f"MinCapacity={sv['minAcu']},MaxCapacity={sv['maxAcu']}"]
    if aws(r["id"], "create-db-cluster", *args) is None:
        continue
    for m in s.get("members") or []:
        aws(r["id"], f"create-db-instance {m['identifier']}", "rds", "create-db-instance", "--db-instance-identifier", m["identifier"],
            "--db-cluster-identifier", s["identifier"], "--engine", s["engine"], "--db-instance-class", m.get("instanceClass") or "db.serverless")
    if s.get("dataApi"):
        aws(r["id"], "enable-http-endpoint", "rds", "enable-http-endpoint", "--resource-arn", r["arn"])

for r in of("rds.instance"):
    s = r["spec"]
    args = ["rds", "create-db-instance", "--db-instance-identifier", s["identifier"], "--engine", s["engine"],
            "--engine-version", s["engineVersion"], "--db-instance-class", s["instanceClass"], *db_auth(s)]
    if s.get("dbName"):
        args += ["--db-name", s["dbName"]]
    if s.get("allocatedGb"):
        args += ["--allocated-storage", str(s["allocatedGb"])]
    if s.get("iamAuth"):
        args += ["--enable-iam-database-authentication"]
    aws(r["id"], "create-db-instance", *args)

# ---------------------------------------------------------------- report
report = {"ok": len(ok), "failed": [dict(zip(("id", "step", "error"), f)) for f in failed],
          "specGaps": [dict(zip(("id", "gap"), g)) for g in gaps]}
json.dump(report, open(os.path.join(os.path.dirname(sys.argv[1]), "seed-report.json"), "w"), indent=2)
print(f"\n{len(ok)} calls ok, {len(failed)} failed, {len(gaps)} spec gaps")
for f in failed:
    print(f"  FAIL {f[0]:<44} {f[1]}\n       {f[2]}")
seen = set()
for g in gaps:
    if g[1] not in seen:
        seen.add(g[1]); print(f"  GAP  {g[1]}   (e.g. {g[0]})")
