#!/usr/bin/env bash
# RDS validation topology, and the configuration and grants that link to it.
#
#   ce-test-db         Aurora PostgreSQL cluster, Serverless v2 at 0–1 ACU: it
#                      pauses when idle, so compute costs nothing between scans.
#                      Created with --with-express-configuration: free-plan
#                      accounts may create Aurora no other way, and express
#                      refuses an initial database, a managed master secret and
#                      the Data API at creation. It adds its own writer,
#                      ce-test-db-instance-1. The Data API is enabled afterwards
#                      where the account allows it.
#   ce-test-legacy-db  a standalone PostgreSQL db.t4g.micro, 20 GB gp3, no
#                      backups, IAM authentication on, and the managed master
#                      secret the cluster could not have.
#
# COSTS MONEY — RDS bills by the hour (the t4g.micro and storage, ~US$0.02/h;
# Aurora only while awake). Tear down with 90-aws-teardown.sh.
#
# Then, once endpoints exist, the linking inputs (merged into each function's
# environment, so the values 10 and 12 set survive):
#   order-processor  DATABASE_URL  → the cluster writer endpoint (was a made-up host)
#                    role: rds-data:ExecuteStatement on the cluster (Data API)
#   orders-fn        DB_READER_HOST → the reader endpoint; DB_SECRET_ARN → the
#                    standalone's managed secret; role: GetSecretValue on it
#   webhook-receiver LEGACY_DB_URL → the standalone instance
# orders-api (ECS) keeps ce-test-db.cluster-abc123…: the cluster's name with
# another account's suffix — the namesake trap, in an endpoint.
source "$(dirname "$0")/lib.sh"
T0=$(date +%s)
R=$AWS_REGION
must() { "$@" || { echo "  FAILED: $*" | cut -c1-160 | redact >&2; exit 1; }; }
TAGS="Key=$TAG_KEY,Value=$TAG_VAL"

log "Aurora PostgreSQL cluster (Serverless v2, 0–1 ACU, express configuration)"
try aws rds create-db-cluster --db-cluster-identifier $P-db --engine aurora-postgresql --with-express-configuration \
  --serverless-v2-scaling-configuration MinCapacity=0,MaxCapacity=1 --tags "$TAGS" >/dev/null || exit 1

log "standalone PostgreSQL instance (db.t4g.micro)"
try aws rds create-db-instance --db-instance-identifier $P-legacy-db --engine postgres \
  --db-instance-class db.t4g.micro --allocated-storage 20 --storage-type gp3 \
  --master-username legacy_admin --manage-master-user-password --db-name legacy \
  --backup-retention-period 0 --no-multi-az --no-publicly-accessible \
  --enable-iam-database-authentication --no-deletion-protection --tags "$TAGS" >/dev/null || exit 1

log "waiting for both to be available (10–15 min)"
must aws rds wait db-instance-available --db-instance-identifier $P-db-instance-1
must aws rds wait db-instance-available --db-instance-identifier $P-legacy-db
echo "  available after $(( $(date +%s) - T0 ))s"
# Serverless v2 and provisioned clusters take EnableHttpEndpoint. The
# modify-db-cluster --enable-http-endpoint flag belongs to Serverless v1: on
# this cluster it returned success and changed nothing — the first scan showed
# the Data API off. It applies asynchronously either way.
CLUSTER_ARN=$(aws rds describe-db-clusters --db-cluster-identifier $P-db --query 'DBClusters[0].DBClusterArn' --output text)
if aws rds enable-http-endpoint --resource-arn "$CLUSTER_ARN" >/dev/null 2>"$WORK/rds-dataapi.err"; then
  echo "  Data API requested on $P-db (applies asynchronously)"
else
  echo "  Data API not enabled: $(tail -1 "$WORK/rds-dataapi.err" | cut -c1-140)"
fi

WRITER=$(aws rds describe-db-clusters --db-cluster-identifier $P-db --query 'DBClusters[0].Endpoint' --output text)
READER=$(aws rds describe-db-clusters --db-cluster-identifier $P-db --query 'DBClusters[0].ReaderEndpoint' --output text)
CLUSTER_ARN=$(aws rds describe-db-clusters --db-cluster-identifier $P-db --query 'DBClusters[0].DBClusterArn' --output text)
SECRET=$(aws rds describe-db-instances --db-instance-identifier $P-legacy-db --query 'DBInstances[0].MasterUserSecret.SecretArn' --output text)
LEGACY=$(aws rds describe-db-instances --db-instance-identifier $P-legacy-db --query 'DBInstances[0].Endpoint.Address' --output text)
for v in "$WRITER" "$READER" "$CLUSTER_ARN" "$SECRET" "$LEGACY"; do
  [ -n "$v" ] && [ "$v" != "None" ] || { echo "  FAILED: an endpoint or ARN is missing" >&2; exit 1; }
done

# setenv merges variables into a function's environment instead of replacing it.
setenv() {
  local fn=$1; shift
  aws lambda get-function-configuration --function-name "$fn" --query 'Environment.Variables' --output json > "$WORK/env-$fn.json"
  python3 - "$WORK/env-$fn.json" "$@" <<'PY'
import json, sys
path, pairs = sys.argv[1], sys.argv[2:]
env = json.load(open(path)) or {}
for p in pairs:
    k, v = p.split("=", 1)
    env[k] = v
json.dump({"Variables": env}, open(path, "w"))
PY
  must aws lambda update-function-configuration --function-name "$fn" --environment "file://$WORK/env-$fn.json" >/dev/null
  aws lambda wait function-updated-v2 --function-name "$fn"
  echo "  $fn: $(printf '%s ' "$@" | sed -E 's/=[^ ]*/=…/g')"
}

log "Tier-2 inputs: configuration naming the databases"
setenv $P-order-processor "DATABASE_URL=postgres://app:not-a-real-password@$WRITER:5432/orders"
setenv $P-orders-fn "DB_READER_HOST=$READER" "DB_SECRET_ARN=$SECRET"
setenv $P-webhook-receiver "LEGACY_DB_URL=postgres://app:not-a-real-password@$LEGACY:5432/legacy"

log "Tier-3 inputs: grants on the cluster"
must aws iam put-role-policy --role-name $P-order-processor-role --policy-name tier3-rds --policy-document \
  "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":\"rds-data:ExecuteStatement\",\"Resource\":\"$CLUSTER_ARN\"}]}"
echo "  $P-order-processor-role / tier3-rds"
# ce-test-apigw-fn-role is shared by orders-fn and authorizer-fn: both get it.
must aws iam put-role-policy --role-name $P-apigw-fn-role --policy-name tier3-rds --policy-document \
  "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":\"secretsmanager:GetSecretValue\",\"Resource\":\"$SECRET\"}]}"
echo "  $P-apigw-fn-role / tier3-rds"

log "done in $(( $(date +%s) - T0 ))s"
