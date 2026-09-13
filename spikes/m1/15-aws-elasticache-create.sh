#!/usr/bin/env bash
# ElastiCache validation topology — one of each of the three APIs ElastiCache
# answers on — and the configuration and grants that link to it.
#
#   ce-test-sessions    Valkey replication group, one cache.t4g.micro node
#                       (DescribeReplicationGroups)
#   ce-test-ratelimit   Valkey Serverless, capped at 1 GB and 1000 ECPU/s
#                       (DescribeServerlessCaches)
#   ce-test-memo        memcached, one cache.t4g.micro node (DescribeCacheClusters)
#
# COSTS MONEY — ElastiCache bills by the hour (~US$0.04/h for the three).
# Tear down with 90-aws-teardown.sh.
#
# Then, once endpoints exist, the linking inputs (merged into each function's
# environment, so earlier values survive):
#   order-processor  SESSIONS_URL   → rediss:// the replication group's primary
#                    role: elasticache:Connect on it (IAM authentication)
#   orders-fn        SESSIONS_READER → its reader endpoint; RATE_LIMIT_HOST →
#                    the serverless cache
#   webhook-receiver MEMO_SERVERS   → the memcached configuration endpoint, host:port
# The twelve notification services keep CACHE_HOST=ce-test-sessions.abc123…:
# the replication group's name with another account's suffix — the namesake trap.
source "$(dirname "$0")/lib.sh"
T0=$(date +%s)
must() { "$@" || { echo "  FAILED: $*" | cut -c1-160 | redact >&2; exit 1; }; }
TAGS="Key=$TAG_KEY,Value=$TAG_VAL"
# The first ElastiCache call in an account creates its service-linked role, and
# creates issued in the next seconds fail with InvalidCredentialsException
# ("cannot be completed now"). Seen on the validation account; retried.
retry() { for i in 1 2 3 4 5 6; do "$@" && return 0; echo "  retrying in 20s"; sleep 20; done; return 1; }

log "cache subnet group (default VPC)"
VPC=$(aws ec2 describe-vpcs --filters Name=is-default,Values=true --query 'Vpcs[0].VpcId' --output text)
SUBNETS=$(aws ec2 describe-subnets --filters Name=vpc-id,Values="$VPC" Name=default-for-az,Values=true --query 'Subnets[0:3].SubnetId' --output text)
try aws elasticache create-cache-subnet-group --cache-subnet-group-name $P-cache-subnets \
  --cache-subnet-group-description "cloud-echo M1 validation" --subnet-ids $SUBNETS --tags "$TAGS" >/dev/null || exit 1

log "Valkey replication group (cache.t4g.micro, one node)"
retry try aws elasticache create-replication-group --replication-group-id $P-sessions \
  --replication-group-description "cloud-echo M1 validation" --engine valkey --cache-node-type cache.t4g.micro \
  --num-cache-clusters 1 --cache-subnet-group-name $P-cache-subnets --transit-encryption-enabled \
  --tags "$TAGS" >/dev/null || exit 1

log "Valkey Serverless (capped)"
retry try aws elasticache create-serverless-cache --serverless-cache-name $P-ratelimit --engine valkey \
  --cache-usage-limits 'DataStorage={Maximum=1,Unit=GB},ECPUPerSecond={Maximum=1000}' \
  --subnet-ids $SUBNETS --tags "$TAGS" >/dev/null || exit 1

log "memcached (cache.t4g.micro, one node)"
retry try aws elasticache create-cache-cluster --cache-cluster-id $P-memo --engine memcached \
  --cache-node-type cache.t4g.micro --num-cache-nodes 1 --cache-subnet-group-name $P-cache-subnets \
  --tags "$TAGS" >/dev/null || exit 1

log "waiting for all three (10–15 min)"
# The CLI's waiter gives up after 10 minutes; a TLS replication group took
# longer on the validation account. Wait again rather than abort half-wired.
retry aws elasticache wait replication-group-available --replication-group-id $P-sessions || exit 1
retry aws elasticache wait cache-cluster-available --cache-cluster-id $P-memo || exit 1
for i in $(seq 1 60); do
  st=$(aws elasticache describe-serverless-caches --serverless-cache-name $P-ratelimit --query 'ServerlessCaches[0].Status' --output text)
  [ "$st" = "available" ] && break; sleep 15
done
[ "$st" = "available" ] || { echo "  FAILED: serverless cache is $st" >&2; exit 1; }
echo "  available after $(( $(date +%s) - T0 ))s"

RG=$(aws elasticache describe-replication-groups --replication-group-id $P-sessions --output json)
PRIMARY=$(jq -r '.ReplicationGroups[0].NodeGroups[0].PrimaryEndpoint.Address' <<<"$RG")
READER=$(jq -r '.ReplicationGroups[0].NodeGroups[0].ReaderEndpoint.Address' <<<"$RG")
RG_ARN=$(jq -r '.ReplicationGroups[0].ARN' <<<"$RG")
SERVERLESS=$(aws elasticache describe-serverless-caches --serverless-cache-name $P-ratelimit --query 'ServerlessCaches[0].Endpoint.Address' --output text)
MEMO=$(aws elasticache describe-cache-clusters --cache-cluster-id $P-memo --query 'CacheClusters[0].ConfigurationEndpoint.Address' --output text)
for v in "$PRIMARY" "$READER" "$RG_ARN" "$SERVERLESS" "$MEMO"; do
  [ -n "$v" ] && [ "$v" != "None" ] && [ "$v" != "null" ] || { echo "  FAILED: an endpoint or ARN is missing" >&2; exit 1; }
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

log "Tier-2 inputs: configuration naming the caches"
setenv $P-order-processor "SESSIONS_URL=rediss://$PRIMARY:6379/0"
setenv $P-orders-fn "SESSIONS_READER=$READER" "RATE_LIMIT_HOST=$SERVERLESS"
setenv $P-webhook-receiver "MEMO_SERVERS=$MEMO:11211"

log "Tier-3 input: IAM authentication to the replication group"
must aws iam put-role-policy --role-name $P-order-processor-role --policy-name tier3-cache --policy-document \
  "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":\"elasticache:Connect\",\"Resource\":\"$RG_ARN\"}]}"
echo "  $P-order-processor-role / tier3-cache"

log "done in $(( $(date +%s) - T0 ))s"
