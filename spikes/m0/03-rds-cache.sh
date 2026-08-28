#!/usr/bin/env bash
# Probe 03 — RDS and ElastiCache: Floci's "real container" claim.
#
# The key questions are not "does the API accept the call" but:
#   1. does a real database container actually appear?
#   2. can a workload container connect to it, and at what hostname?
# Question 2 decides what `${ref:rds/x.host}` resolves to in the blueprint.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

section "03 · rds + elasticache"

# --- rds ---------------------------------------------------------------------
# Engine version deliberately unpinned: we want to see what Floci defaults to.
if out=$(awsx rds create-db-instance \
    --db-instance-identifier ce-orders-db \
    --db-instance-class db.t3.micro \
    --engine postgres \
    --master-username postgres \
    --master-user-password localonly123 \
    --allocated-storage 20 \
    --db-name orders \
    --output json 2>&1); then
  record PASS rds.create "postgres instance requested"
  save "rds-create.json" "$out"
else
  record FAIL rds.create "$(last_line "$out")"
fi

info "waiting for RDS to report available (up to 180s)..."
rds_available() {
  [ "$(awsx rds describe-db-instances --db-instance-identifier ce-orders-db \
       --query 'DBInstances[0].DBInstanceStatus' --output text 2>/dev/null)" = "available" ]
}
if wait_for 180 rds_available; then
  record PASS rds.available "reached available"
else
  record FAIL rds.available "never reached available within 180s"
fi

desc=$(awsx rds describe-db-instances --db-instance-identifier ce-orders-db --output json 2>&1)
save "rds-describe.json" "$desc"

rds_host=$(printf '%s' "$desc" | jq -r '.DBInstances[0].Endpoint.Address // ""')
rds_port=$(printf '%s' "$desc" | jq -r '.DBInstances[0].Endpoint.Port // ""')
rds_ver=$(printf '%s' "$desc" | jq -r '.DBInstances[0].EngineVersion // "?"')

if [ -n "$rds_host" ]; then
  record PASS rds.endpoint "$rds_host:$rds_port (engine $rds_ver)"
else
  record FAIL rds.endpoint "no endpoint reported"
fi

# Did a real postgres container appear?
pg_container=$(docker ps --filter 'ancestor=postgres' --format '{{.Names}}' | head -1)
if [ -z "$pg_container" ]; then
  pg_container=$(docker ps --format '{{.Names}}\t{{.Image}}' | grep -i 'postgres\|mysql\|maria' | head -1 | cut -f1)
fi
if [ -n "$pg_container" ]; then
  ports=$(docker port "$pg_container" 2>/dev/null | tr '\n' ' ')
  record PASS rds.container "$pg_container · ports: ${ports:-none published}"
  save "rds-container-inspect.json" "$(docker inspect "$pg_container")"

  # Which docker network is it on? Workloads must be able to reach it.
  nets=$(docker inspect "$pg_container" --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}')
  if printf '%s' "$nets" | grep -q "$FLOCI_NETWORK"; then
    record PASS rds.network "on $FLOCI_NETWORK — workloads can reach it directly"
  else
    record PARTIAL rds.network "on [$nets], NOT on $FLOCI_NETWORK — materializer must attach it"
  fi
else
  record FAIL rds.container "no database container found — RDS may be mocked, not real"
fi

# --- can we actually speak Postgres to it? -----------------------------------
# Try from a sibling container on the same network, which is how a workload
# would connect. This is the answer to "what does ${ref:rds/x.host} resolve to".
rm -f "$WORK_DIR/rds-resolved-host.txt"
if [ -n "$rds_host" ]; then
  for candidate in "$rds_host" "$pg_container" "$FLOCI_CONTAINER" "host.docker.internal"; do
    [ -z "$candidate" ] && continue
    if docker run --rm --network "$FLOCI_NETWORK" \
        -e PGPASSWORD=localonly123 postgres:16-alpine \
        psql -h "$candidate" -p "${rds_port:-5432}" -U postgres -d orders \
        -c 'select 1' >/dev/null 2>&1; then
      record PASS rds.connect "connected as '$candidate' from a sibling container"
      save "rds-resolved-host.txt" "$candidate"
      break
    fi
    info "  no route via '$candidate'"
  done
  [ -f "$WORK_DIR/rds-resolved-host.txt" ] \
    || record FAIL rds.connect "no hostname worked from inside $FLOCI_NETWORK — blocks \${ref:rds/*.host}"
fi

# --- elasticache -------------------------------------------------------------
# Floci rejects Redis/Valkey on CreateCacheCluster ("Engine must be 'memcached'.
# For Redis/Valkey use CreateReplicationGroup"), matching modern AWS. The
# discovery collector must therefore read DescribeReplicationGroups for
# Redis/Valkey and DescribeCacheClusters only for memcached.
if awsx elasticache create-replication-group \
    --replication-group-id ce-sessions \
    --replication-group-description "cloud-echo m0 probe" \
    --engine valkey --cache-node-type cache.t3.micro \
    --num-cache-clusters 1 >/dev/null 2>&1; then
  record PASS cache.create "valkey via CreateReplicationGroup"
else
  record FAIL cache.create "CreateReplicationGroup rejected"
fi

rg_available() {
  [ "$(awsx elasticache describe-replication-groups --replication-group-id ce-sessions \
       --query 'ReplicationGroups[0].Status' --output text 2>/dev/null)" = "available" ]
}
wait_for 60 rg_available \
  && record PASS cache.available "replication group available" \
  || record PARTIAL cache.available "never reached available"

cdesc=$(awsx elasticache describe-replication-groups --replication-group-id ce-sessions --output json 2>&1)
save "cache-describe.json" "$cdesc"

cache_host=$(printf '%s' "$cdesc" | jq -r '.ReplicationGroups[0].NodeGroups[0].PrimaryEndpoint.Address // ""')
cache_port=$(printf '%s' "$cdesc" | jq -r '.ReplicationGroups[0].NodeGroups[0].PrimaryEndpoint.Port // "6379"')
if [ -n "$cache_host" ]; then
  record PASS cache.endpoint "$cache_host:$cache_port"
else
  # Observed: Floci reports NodeGroups: null, so there is no discoverable
  # endpoint. Fall back to the Floci host, which proxies the connection.
  cache_host="$FLOCI_CONTAINER"; cache_port=6379
  record FAIL cache.endpoint "NodeGroups is null — no endpoint via the API; blueprint must synthesise one"
fi

valkey_container=$(docker ps --format '{{.Names}}\t{{.Image}}' | grep -i 'valkey\|redis' | head -1 | cut -f1)
[ -n "$valkey_container" ] \
  && record PASS cache.container "$valkey_container" \
  || record FAIL cache.container "no cache container found — ElastiCache may be mocked"

# Note: Floci documents SigV4 validation on ElastiCache. A plain redis-cli PING
# may be rejected; record whichever behaviour we see rather than assuming.
if [ -n "$cache_host" ]; then
  if out=$(docker run --rm --network "$FLOCI_NETWORK" valkey/valkey:8 \
      valkey-cli -h "$cache_host" -p "$cache_port" ping 2>&1); then
    record PASS cache.connect "PING → $(last_line "$out")"
  else
    record PARTIAL cache.connect "unauthenticated PING failed: $(last_line "$out")"
  fi
fi
