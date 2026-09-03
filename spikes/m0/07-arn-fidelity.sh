#!/usr/bin/env bash
# Probe 07 — ARN fidelity.
#
# docs/01-architecture.md claims that injecting the real 12-digit account ID as
# AWS_ACCESS_KEY_ID makes local ARNs byte-identical to production ARNs, so apps
# with hardcoded ARNs work unmodified. Verify it across every service, not just
# STS — one service that hardcodes 000000000000 breaks the claim.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

section "07 · ARN fidelity"

check_arn() {
  local label="$1" arn="$2" expected_re="$3"
  if [ -z "$arn" ] || [ "$arn" = "None" ] || [ "$arn" = "null" ]; then
    record UNKNOWN "arn.$label" "resource not present (earlier probe failed?)"
    return
  fi
  printf '%s\n' "$arn" >> "$WORK_DIR/arns.txt"
  if printf '%s' "$arn" | grep -Eq "$expected_re"; then
    record PASS "arn.$label" "$arn"
  else
    record FAIL "arn.$label" "$arn  (expected to match $expected_re)"
  fi
}

: > "$WORK_DIR/arns.txt"
A="$ACCOUNT_ID"
R="$AWS_REGION"

check_arn dynamodb \
  "$(awsx dynamodb describe-table --table-name ce-orders --query 'Table.TableArn' --output text 2>/dev/null)" \
  "^arn:aws:dynamodb:${R}:${A}:table/ce-orders$"

check_arn sqs \
  "$(awsx sqs get-queue-attributes --queue-url "$(cat "$WORK_DIR/queue-url.txt" 2>/dev/null)" \
     --attribute-names QueueArn --query 'Attributes.QueueArn' --output text 2>/dev/null)" \
  "^arn:aws:sqs:${R}:${A}:ce-orders$"

check_arn lambda \
  "$(awsx lambda get-function-configuration --function-name ce-fn --query FunctionArn --output text 2>/dev/null)" \
  "^arn:aws:lambda:${R}:${A}:function:ce-fn$"

check_arn ecs-service \
  "$(awsx ecs describe-services --cluster ce-m0 --services ce-probe-svc \
     --query 'services[0].serviceArn' --output text 2>/dev/null)" \
  "^arn:aws:ecs:${R}:${A}:service/"

check_arn secret \
  "$(awsx secretsmanager describe-secret --secret-id ce/orders/db --query ARN --output text 2>/dev/null)" \
  "^arn:aws:secretsmanager:${R}:${A}:secret:"

check_arn iam-role \
  "$(awsx iam get-role --role-name ce-lambda-role --query 'Role.Arn' --output text 2>/dev/null)" \
  "^arn:aws:iam::${A}:role/ce-lambda-role$"

# --- SQS queue URL shape -----------------------------------------------------
# Distinct from ARNs and just as important: applications store queue URLs in
# env vars, and the linker matches on the production URL pattern.
qurl=$(cat "$WORK_DIR/queue-url.txt" 2>/dev/null)
if printf '%s' "$qurl" | grep -Eq "^https?://.*/${A}/ce-orders$"; then
  record PASS url.sqs "$qurl (contains the real account id)"
else
  record PARTIAL url.sqs "$qurl — differs from the AWS shape; blueprint must rewrite QUEUE_URL env vars"
fi

info "all collected ARNs → $WORK_DIR/arns.txt"

# --- account isolation -------------------------------------------------------
# Floci claims per-account isolation. cloud-echo does not depend on it today,
# but it would let one Floci back several projects.
iso=$(AWS_ACCESS_KEY_ID=999999999999 aws --endpoint-url "$AWS_ENDPOINT_URL" \
      --no-cli-pager dynamodb list-tables --output json 2>/dev/null | jq -r '.TableNames | length')
if [ "$iso" = "0" ]; then
  record PASS account.isolation "a different account id sees 0 tables — isolation works"
else
  record PARTIAL account.isolation "other account sees $iso tables — no isolation; one Floci per project"
fi
