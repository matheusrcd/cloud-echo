#!/usr/bin/env bash
# Round trip: inventory from AWS → seed a local Floci → scan Floci → compare.
#
# Floci runs with the *real* account id as its access key, so ARNs created
# locally are byte-identical to production ones (docs/01-architecture.md) and the
# two inventories can be compared field by field.
source "$(dirname "$0")/lib.sh"
M0="$HERE/../m0"
export FLOCI_CONTAINER=ce-m1-floci FLOCI_NETWORK=ce-m1 FLOCI_VOLUME=ce-m1-data DOCKER_PROXY_CONTAINER=ce-m1-dockerproxy
IN="${1:-$WORK/inventory-aws.json}"

log "Floci up (account = scanned account, region $AWS_REGION) — from an empty volume"
# M0 mounts a volume so state persists; a round trip must start from nothing.
docker rm -f "$FLOCI_CONTAINER" >/dev/null 2>&1; docker volume rm "$FLOCI_VOLUME" >/dev/null 2>&1
( CE_ACCOUNT_ID="$ACCT" AWS_DEFAULT_REGION="$AWS_REGION" bash "$M0/01-floci-up.sh" ) 2>&1 | grep -E 'PASS|FAIL|PARTIAL|ready' | redact

log "seed Floci from the AWS inventory"
T0=$(date +%s); python3 "$HERE/seed-floci.py" "$IN" | redact; echo "  seeded in $(( $(date +%s) - T0 ))s"

log "scan Floci with cloud-echo"
env -u AWS_PROFILE AWS_ENDPOINT_URL=http://localhost:4566 AWS_ACCESS_KEY_ID="$ACCT" AWS_SECRET_ACCESS_KEY=test \
  AWS_CONFIG_FILE=/dev/null AWS_SHARED_CREDENTIALS_FILE=/dev/null \
  "$WORK/cloud-echo" scan --region "$AWS_REGION" --out "$WORK/inventory-floci.json" 2>&1 | redact

log "compare AWS vs Floci (spec level)"
python3 "$HERE/diff-inventory.py" "$IN" "$WORK/inventory-floci.json" --ignore-raw | redact
