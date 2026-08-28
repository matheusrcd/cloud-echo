#!/usr/bin/env bash
# Shared config and helpers for the M0 spike.
#
# Deliberately NOT `set -e`: a probe that fails is a finding, not a crash. Every
# probe records its own outcome and the script continues.
set -uo pipefail

# ---- config -----------------------------------------------------------------
export FLOCI_IMAGE="${FLOCI_IMAGE:-floci/floci:latest}"
export FLOCI_CONTAINER="${FLOCI_CONTAINER:-ce-m0-floci}"
export FLOCI_PORT="${FLOCI_PORT:-4566}"
export FLOCI_NETWORK="${FLOCI_NETWORK:-ce-m0}"
export FLOCI_VOLUME="${FLOCI_VOLUME:-ce-m0-data}"

# Floci's docker-java client cannot open the unix socket directly on Docker
# Desktop for macOS — it fails with `BindException: Permission denied` against
# unix://localhost:2375. A socat bridge exposing the socket over TCP on the
# project network is the workaround. Reachable only inside the docker network,
# never published to the host. See docs/spikes/m0-findings.md D6.
export DOCKER_PROXY_CONTAINER="${DOCKER_PROXY_CONTAINER:-ce-m0-dockerproxy}"

# Exactly 12 digits, on purpose: this exercises Floci's account-id behaviour so
# probe 07 can check that local ARNs are byte-identical to production ARNs.
export CE_ACCOUNT_ID="${CE_ACCOUNT_ID:-123456789012}"
export AWS_ACCESS_KEY_ID="$CE_ACCOUNT_ID"
export AWS_SECRET_ACCESS_KEY="test"
export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-east-1}"
export AWS_REGION="$AWS_DEFAULT_REGION"
export AWS_ENDPOINT_URL="http://localhost:${FLOCI_PORT}"
export ACCOUNT_ID="$CE_ACCOUNT_ID"

SPIKE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export SPIKE_DIR
export WORK_DIR="$SPIKE_DIR/.work"
mkdir -p "$WORK_DIR"
export FINDINGS="$WORK_DIR/findings.tsv"

# ---- output -----------------------------------------------------------------
if [ -t 1 ]; then
  c_reset=$'\033[0m'; c_red=$'\033[31m'; c_grn=$'\033[32m'
  c_yel=$'\033[33m'; c_blu=$'\033[34m'; c_dim=$'\033[2m'
else
  c_reset=""; c_red=""; c_grn=""; c_yel=""; c_blu=""; c_dim=""
fi

section() { printf '\n%s━━ %s%s\n' "$c_blu" "$*" "$c_reset"; }
info()    { printf '%s   · %s%s\n' "$c_dim" "$*" "$c_reset"; }

# record <PASS|FAIL|PARTIAL|UNKNOWN> <probe-id> <note...>
record() {
  local status="$1" id="$2"; shift 2
  local color
  case "$status" in
    PASS)    color="$c_grn" ;;
    FAIL)    color="$c_red" ;;
    *)       color="$c_yel" ;;
  esac
  printf '   %s[%-7s]%s %-26s %s\n' "$color" "$status" "$c_reset" "$id" "$*"
  printf '%s\t%s\t%s\n' "$status" "$id" "$*" >> "$FINDINGS"
}

need() {
  command -v "$1" >/dev/null 2>&1 || {
    printf '%smissing required tool: %s%s\n' "$c_red" "$1" "$c_reset" >&2
    exit 1
  }
}

# AWS CLI pointed at Floci, never at real AWS.
awsx() { aws --endpoint-url "$AWS_ENDPOINT_URL" --no-cli-pager "$@"; }

# last_line <text> — trim a multi-line error down to something loggable
last_line() { printf '%s' "$1" | tr '\n' ' ' | tail -c 160; }

# save <name> <content> — keep raw API output for the findings write-up
save() { printf '%s\n' "$2" > "$WORK_DIR/$1"; }

# wait_for <seconds> <command...> — poll until the command succeeds
wait_for() {
  local timeout="$1"; shift
  local deadline=$(( SECONDS + timeout ))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  return 1
}
