#!/usr/bin/env bash
# Shared config for the M1 real-account validation.
#
# Everything this suite creates is prefixed ce-test- and tagged
# cloud-echo-test=m1, and 90-aws-teardown.sh removes exactly that set. The
# account id is always read at runtime and never written to a committed file.
set -uo pipefail

export AWS_PROFILE="${AWS_PROFILE:-cloud-echo}"
export AWS_REGION="${AWS_REGION:-us-east-2}"
export AWS_PAGER=""

export P="ce-test"
export TAG_KEY="cloud-echo-test"
export TAG_VAL="m1"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export HERE
export WORK="$HERE/.work"
mkdir -p "$WORK"

ACCT="$(aws sts get-caller-identity --query Account --output text)" || {
  echo "cannot resolve the account — are the credentials for profile $AWS_PROFILE still valid?" >&2
  exit 1
}
export ACCT

# redact hides the account id in anything printed, so logs are safe to paste.
redact() { sed -E "s/$ACCT/<ACCT>/g"; }

log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

# try runs a create call and treats "already exists" as success, so the
# create script can be re-run after a partial failure.
try() {
  local out
  if out="$("$@" 2>&1)"; then
    return 0
  fi
  if grep -qiE 'already exists|already enabled|EntityAlreadyExists|ResourceInUse|ResourceConflict|QueueAlreadyExists|InvalidParameterException.*exists' <<<"$out"; then
    echo "  (exists) $*" | cut -c1-120 | redact
    return 0
  fi
  echo "  FAILED: $*" | cut -c1-160 | redact >&2
  echo "$out" | redact | sed 's/^/    /' >&2
  return 1
}
