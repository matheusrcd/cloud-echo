#!/usr/bin/env bash
# Probe 00 — is this machine able to run the spike at all?
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

section "00 · preflight"

need docker; need aws; need jq

# --- docker daemon -----------------------------------------------------------
if docker info >/dev/null 2>&1; then
  record PASS docker.daemon "$(docker version --format '{{.Server.Version}}' 2>/dev/null)"
else
  record FAIL docker.daemon "daemon unreachable — start Docker Desktop / colima and re-run"
  exit 1
fi

# --- host architecture -------------------------------------------------------
# Matters because ECR images in real accounts are almost always linux/amd64 and
# most developers are now on arm64. See docs/05-materializer.md#images.
HOST_ARCH="$(uname -m)"
if [ "$HOST_ARCH" = "arm64" ] || [ "$HOST_ARCH" = "aarch64" ]; then
  record PARTIAL host.arch "$HOST_ARCH — amd64 images will run emulated; measure the cost in probe 08"
else
  record PASS host.arch "$HOST_ARCH"
fi

# --- docker socket path ------------------------------------------------------
# Floci needs the socket to launch real containers for ECS/Lambda/RDS.
SOCK="/var/run/docker.sock"
if [ -S "$SOCK" ]; then
  record PASS docker.socket "$SOCK"
else
  # Probes run as separate processes, so this has to be persisted, not exported.
  ALT="${DOCKER_HOST#unix://}"
  if [ -z "$ALT" ] && [ -S "$HOME/.docker/run/docker.sock" ]; then
    ALT="$HOME/.docker/run/docker.sock"
  fi
  if [ -n "$ALT" ] && [ -S "$ALT" ]; then
    record PARTIAL docker.socket "$SOCK missing; using $ALT instead"
    save "docker-sock.txt" "$ALT"
  else
    record FAIL docker.socket "no usable docker socket; ECS/Lambda/RDS probes will fail"
  fi
fi

# --- ports -------------------------------------------------------------------
for port in "$FLOCI_PORT" 5432 6379; do
  if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    record PARTIAL "port.$port" "already in use — may collide"
  else
    record PASS "port.$port" "free"
  fi
done

# --- aws cli -----------------------------------------------------------------
CLI_VER="$(aws --version 2>&1 | head -1)"
if aws --version 2>&1 | grep -q 'aws-cli/2'; then
  record PASS aws.cli "$CLI_VER"
else
  record FAIL aws.cli "$CLI_VER — v2 required (AWS_ENDPOINT_URL support)"
fi

# --- image pull --------------------------------------------------------------
section "00 · pulling $FLOCI_IMAGE"
pull_start=$SECONDS
if out=$(docker pull "$FLOCI_IMAGE" 2>&1); then
  record PASS floci.pull "$(( SECONDS - pull_start ))s · $(docker image inspect "$FLOCI_IMAGE" --format '{{.Size}}' | awk '{printf "%.0f MB", $1/1024/1024}')"
else
  record FAIL floci.pull "$(last_line "$out")"
  exit 1
fi

# Images the probes need. Pulled up front so probe timings measure Floci, not
# the network.
for img in alpine:3.20 postgres:16-alpine; do
  docker pull "$img" >/dev/null 2>&1 \
    && record PASS "pull.${img%%:*}" "$img" \
    || record PARTIAL "pull.${img%%:*}" "$img failed — some probes will be skipped"
done
