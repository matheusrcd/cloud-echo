#!/usr/bin/env bash
# Probe 01 — start Floci and find out how we detect readiness.
#
# The materializer needs a reliable readiness signal (docs/05-materializer.md
# step 3). Floci's site advertises a ~24ms cold start but documents no health
# endpoint, so we discover one here.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

section "01 · floci lifecycle"

docker rm -f "$FLOCI_CONTAINER" >/dev/null 2>&1
# Docker does not guarantee unique network names: `docker network create` with a
# name that already exists can succeed and make a second network. Found in M1,
# after three runs left three networks called the same thing and `docker run
# --network` refused to choose. Look the network up; never create blindly.
nets=$(docker network ls -q --filter "name=^${FLOCI_NETWORK}$")
case $(printf '%s' "$nets" | grep -c .) in
  0) docker network create "$FLOCI_NETWORK" >/dev/null && info "created network $FLOCI_NETWORK" ;;
  1) info "network $FLOCI_NETWORK already exists" ;;
  *) record FAIL floci.network "$(printf '%s' "$nets" | grep -c .) networks named $FLOCI_NETWORK — remove the duplicates"; exit 1 ;;
esac

# Probe 00 records an alternative socket path here when /var/run/docker.sock is
# absent (common with Docker Desktop and colima on macOS).
SOCK="$(cat "$WORK_DIR/docker-sock.txt" 2>/dev/null || echo /var/run/docker.sock)"
info "mounting docker socket: $SOCK"

# --- docker socket bridge ----------------------------------------------------
# Floci's docker-java client cannot use the mounted unix socket directly on
# Docker Desktop for macOS (BindException: Permission denied). Bridge it over
# TCP on the project network only — never published to the host.
docker rm -f "$DOCKER_PROXY_CONTAINER" >/dev/null 2>&1
if docker run -d --name "$DOCKER_PROXY_CONTAINER" --network "$FLOCI_NETWORK" \
    -v "${SOCK}:/var/run/docker.sock" alpine/socat \
    TCP-LISTEN:2375,fork,reuseaddr UNIX-CONNECT:/var/run/docker.sock >/dev/null 2>&1; then
  record PASS docker.proxy "socat bridge on ${DOCKER_PROXY_CONTAINER}:2375 (network-internal only)"
else
  record FAIL docker.proxy "could not start the socat bridge — container services will fail"
fi

# --- persistent storage ------------------------------------------------------
# FLOCI_STORAGE_MODE alone does not survive a restart: without a mounted volume
# the state directory is inside the container's writable layer. The volume must
# also be group-writable or Floci's boot-time write probe fails.
docker volume create "$FLOCI_VOLUME" >/dev/null 2>&1
docker run --rm -v "${FLOCI_VOLUME}:/data" alpine:3.20 \
  sh -c 'chown -R 0:0 /data && chmod -R 775 /data' >/dev/null 2>&1

start_ts=$(python3 -c 'import time; print(time.time())')
if out=$(docker run -d \
    --name "$FLOCI_CONTAINER" \
    --network "$FLOCI_NETWORK" \
    -p "${FLOCI_PORT}:4566" \
    -v "${SOCK}:/var/run/docker.sock" \
    -v "${FLOCI_VOLUME}:/app/data" \
    -e DOCKER_HOST="tcp://${DOCKER_PROXY_CONTAINER}:2375" \
    -e FLOCI_DEFAULT_REGION="$AWS_REGION" \
    -e FLOCI_STORAGE_MODE=hybrid \
    "$FLOCI_IMAGE" 2>&1); then
  record PASS floci.start "container ${out:0:12}"
else
  record FAIL floci.start "$(last_line "$out")"
  exit 1
fi

# --- readiness ---------------------------------------------------------------
# First successful API call is the only signal we can rely on for certain.
if wait_for 60 awsx sts get-caller-identity; then
  end_ts=$(python3 -c 'import time; print(time.time())')
  boot=$(python3 -c "print(f'{($end_ts - $start_ts)*1000:.0f}')")
  record PASS floci.ready "first successful API call after ${boot}ms (includes docker run)"
else
  record FAIL floci.ready "no API response within 60s"
  docker logs "$FLOCI_CONTAINER" 2>&1 | tail -30 > "$WORK_DIR/floci-boot.log"
  info "logs → $WORK_DIR/floci-boot.log"
  exit 1
fi

# --- who does Floci think we are? -------------------------------------------
ident=$(awsx sts get-caller-identity --output json 2>&1)
save "sts-identity.json" "$ident"
got_account=$(printf '%s' "$ident" | jq -r '.Account // "?"')
if [ "$got_account" = "$CE_ACCOUNT_ID" ]; then
  record PASS floci.account-id "12-digit access key adopted as account $got_account"
else
  record FAIL floci.account-id "expected $CE_ACCOUNT_ID, got $got_account — the ARN-fidelity trick does not work"
fi

# --- health endpoint discovery ----------------------------------------------
# Try the known LocalStack-compatible paths plus plausible Floci-native ones.
found_health=""
for path in /_localstack/health /_floci/health /health /q/health/ready /_floci/status; do
  code=$(curl -s -o "$WORK_DIR/health-body.txt" -w '%{http_code}' \
         "http://localhost:${FLOCI_PORT}${path}" 2>/dev/null)
  if [ "$code" = "200" ]; then
    found_health="$path"
    save "health-response.json" "$(cat "$WORK_DIR/health-body.txt")"
    break
  fi
done

if [ -n "$found_health" ]; then
  record PASS floci.health "$found_health → 200 (use this for readiness)"
else
  record PARTIAL floci.health "no health endpoint found; fall back to polling sts:GetCallerIdentity"
fi

# --- what is Floci's hostname on our network? --------------------------------
# Workload containers reach Floci by container name, not localhost.
net_ip=$(docker inspect "$FLOCI_CONTAINER" \
  --format "{{ (index .NetworkSettings.Networks \"$FLOCI_NETWORK\").IPAddress }}" 2>/dev/null)
if [ -n "$net_ip" ]; then
  record PASS floci.network "reachable at $FLOCI_CONTAINER ($net_ip) on $FLOCI_NETWORK"
else
  record PARTIAL floci.network "could not resolve container IP on $FLOCI_NETWORK"
fi

# --- does Floci put its own child containers on our network? -----------------
# Recorded now as a baseline; probes 03/04/05 check it again once children exist.
save "containers-before.txt" "$(docker ps --format '{{.Names}}\t{{.Image}}')"
info "container baseline → $WORK_DIR/containers-before.txt"
