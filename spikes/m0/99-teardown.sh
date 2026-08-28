#!/usr/bin/env bash
# Teardown — remove everything the spike created.
#
# Floci's child containers (RDS, Valkey, Lambda runtimes, ECS tasks) are not
# owned by us, so they are matched by image rather than by label. This is
# exactly why docs/05-materializer.md requires cloud-echo to label everything it
# creates: reliable cleanup is otherwise guesswork.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

section "99 · teardown"

for c in "$FLOCI_CONTAINER" "$DOCKER_PROXY_CONTAINER"; do
  docker rm -f "$c" >/dev/null 2>&1 && info "removed $c" || info "$c not running"
done

# Floci labels everything it spawns `floci=true` — the reliable way to reclaim
# its children. (Confirmed in M0; this is the pattern cloud-echo copies.)
labelled=$(docker ps -aq --filter 'label=floci=true' 2>/dev/null)
if [ -n "$labelled" ]; then
  # shellcheck disable=SC2086
  docker rm -f $labelled >/dev/null 2>&1
  info "removed $(printf '%s\n' "$labelled" | grep -c .) floci-labelled container(s)"
fi

# Belt and braces for anything the label missed.
orphans=$(docker ps -aq --filter 'ancestor=postgres:16-alpine' \
                       --filter 'ancestor=valkey/valkey:8' \
                       --filter 'ancestor=alpine:3.20' 2>/dev/null)
if [ -n "$orphans" ]; then
  # shellcheck disable=SC2086
  docker rm -f $orphans >/dev/null 2>&1
  info "removed $(printf '%s\n' "$orphans" | grep -c .) orphaned container(s)"
fi

docker network rm "$FLOCI_NETWORK" >/dev/null 2>&1 \
  && info "removed network $FLOCI_NETWORK" \
  || info "network $FLOCI_NETWORK still in use or absent"

docker volume rm "$FLOCI_VOLUME" >/dev/null 2>&1 \
  && info "removed volume $FLOCI_VOLUME" \
  || info "volume $FLOCI_VOLUME absent or in use"

if [ "${1:-}" = "--all" ]; then
  rm -rf "$WORK_DIR"
  info "removed $WORK_DIR (findings and raw API output are gone)"
else
  info "kept $WORK_DIR — raw API output and findings.tsv are the spike's real output"
  info "pass --all to delete it"
fi
