#!/usr/bin/env bash
# Probe 08 — resource footprint.
#
# docs/05-materializer.md promises `up` estimates memory and refuses to start if
# the slice will not fit. That needs real numbers: how much does a materialized
# slice actually cost on a laptop?
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

section "08 · footprint"

stats=$(docker stats --no-stream --format '{{.Name}}\t{{.MemUsage}}\t{{.CPUPerc}}' 2>/dev/null)
save "docker-stats.txt" "$stats"
printf '%s\n' "$stats" | sed 's/^/     /'

count=$(printf '%s\n' "$stats" | grep -c . )
record PASS footprint.containers "$count containers running for one small slice"

# Total memory across every container (MiB).
total_mb=$(printf '%s\n' "$stats" | awk -F'\t' '{split($2,a," / "); v=a[1];
  if (v ~ /GiB/) {gsub("GiB","",v); s+=v*1024}
  else if (v ~ /MiB/) {gsub("MiB","",v); s+=v}
  else if (v ~ /KiB/) {gsub("KiB","",v); s+=v/1024}
} END {printf "%.0f", s}')
record PASS footprint.memory "${total_mb} MiB total"

host_mb=$(( $(sysctl -n hw.memsize 2>/dev/null || echo 0) / 1024 / 1024 ))
[ "$host_mb" -gt 0 ] && info "host memory: ${host_mb} MiB — slice uses $(( total_mb * 100 / host_mb ))%"

# Image disk cost. Real ECR images are far larger than alpine; this is a floor.
img_mb=$(docker image inspect "$FLOCI_IMAGE" --format '{{.Size}}' 2>/dev/null | awk '{printf "%.0f", $1/1024/1024}')
record PASS footprint.floci-image "${img_mb} MiB"

# --- restart cost ------------------------------------------------------------
# hybrid storage should survive a restart; the M6 iteration loop depends on it.
before=$(awsx dynamodb list-tables --query 'TableNames' --output json 2>/dev/null)
docker restart "$FLOCI_CONTAINER" >/dev/null 2>&1
restart_start=$SECONDS
if wait_for 60 awsx sts get-caller-identity; then
  record PASS footprint.restart "back up in $(( SECONDS - restart_start ))s"
  after=$(awsx dynamodb list-tables --query 'TableNames' --output json 2>/dev/null)
  if [ "$before" = "$after" ]; then
    record PASS footprint.persistence "tables survived the restart (storage=hybrid)"
  else
    record FAIL footprint.persistence "state lost on restart — before=$before after=$after"
  fi
else
  record FAIL footprint.restart "did not come back within 60s"
fi
