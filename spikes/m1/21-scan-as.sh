#!/usr/bin/env bash
# Usage: 21-scan-as.sh <role-name> <out.json>
# Assumes the role and runs cloud-echo with only those credentials.
source "$(dirname "$0")/lib.sh"
ROLE=$1 OUT=$2
for i in $(seq 1 10); do   # new roles take a moment to become assumable
  CREDS=$(aws sts assume-role --role-arn "arn:aws:iam::$ACCT:role/$ROLE" --role-session-name cloud-echo-m1 \
    --query 'Credentials.[AccessKeyId,SecretAccessKey,SessionToken]' --output text 2>/dev/null) && break
  sleep 3
done
[ -n "${CREDS:-}" ] || { echo "could not assume $ROLE" >&2; exit 1; }
read -r AK SK ST <<<"$CREDS"
# Credentials live only in this process's environment; nothing is written.
env -u AWS_PROFILE AWS_ACCESS_KEY_ID="$AK" AWS_SECRET_ACCESS_KEY="$SK" AWS_SESSION_TOKEN="$ST" \
  "$WORK/cloud-echo" scan --region "$AWS_REGION" --out "$OUT" 2>&1 | redact
