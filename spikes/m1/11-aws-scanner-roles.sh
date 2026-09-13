#!/usr/bin/env bash
# Two roles to scan *as*, so the shipped policy is tested rather than assumed:
#
#   ce-test-scanner          exactly policies/cloud-echo-scanner.json
#   ce-test-scanner-partial  the same, plus explicit Denies — proves degradation
#                            against the error codes AWS really returns
source "$(dirname "$0")/lib.sh"
ROOT="$(cd "$HERE/../.." && pwd)"
TRUST="{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Principal\":{\"AWS\":\"arn:aws:iam::$ACCT:root\"},\"Action\":\"sts:AssumeRole\"}]}"

log "scanner roles"
try aws iam create-role --role-name $P-scanner --assume-role-policy-document "$TRUST" --tags "Key=$TAG_KEY,Value=$TAG_VAL"
try aws iam put-role-policy --role-name $P-scanner --policy-name cloud-echo-scanner \
  --policy-document "file://$ROOT/policies/cloud-echo-scanner.json"

try aws iam create-role --role-name $P-scanner-partial --assume-role-policy-document "$TRUST" --tags "Key=$TAG_KEY,Value=$TAG_VAL"
try aws iam put-role-policy --role-name $P-scanner-partial --policy-name cloud-echo-scanner \
  --policy-document "file://$ROOT/policies/cloud-echo-scanner.json"
try aws iam put-role-policy --role-name $P-scanner-partial --policy-name deny-some --policy-document \
  '{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":["iam:GetRole","lambda:GetPolicy"],"Resource":"*"}]}'
echo "  ok"
