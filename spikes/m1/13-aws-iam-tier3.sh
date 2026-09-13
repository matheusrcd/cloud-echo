#!/usr/bin/env bash
# Tier-3 inputs on top of the M1 topology: grants the linker must cancel, widen
# or read the other way. Each is a separate inline policy, so the roles
# 10-aws-create.sh defines keep their own documents, and re-running this script
# just rewrites the same four policies.
#
#   orders-api-task   lambda:InvokeFunction on orders-fn — its permissions
#                     boundary allows no lambda:*, so this must not become an edge
#   webhook-receiver  dynamodb:PutItem on the orders table — cancelled by the
#                     explicit Deny dynamodb:* in its enqueue policy
#   notifications     sqs:ReceiveMessage on its FIFO queue — a worker that polls
#                     the queue it names (Q17), beside AmazonSQSFullAccess
#   order-processor   dynamodb:GetItem on table/ce-test-orders* — a pattern that
#                     matches two tables, so two low candidates
#
# IAM is free; nothing here runs.
source "$(dirname "$0")/lib.sh"
T0=$(date +%s)
QA="arn:aws:sqs:$AWS_REGION:$ACCT"
DA="arn:aws:dynamodb:$AWS_REGION:$ACCT:table"
FA="arn:aws:lambda:$AWS_REGION:$ACCT:function"
must() { "$@" || { echo "  FAILED: $*" | cut -c1-160 | redact >&2; exit 1; }; }
put() {
  must aws iam put-role-policy --role-name "$1" --policy-name "$2" --policy-document "$3"
  echo "  $1 / $2"
}

log "Tier-3 inline policies"
put $P-orders-api-task tier3-invoke \
  "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":\"lambda:InvokeFunction\",\"Resource\":\"$FA:$P-orders-fn\"}]}"
put $P-webhook-receiver-role tier3-write \
  "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":\"dynamodb:PutItem\",\"Resource\":\"$DA/$P-orders\"}]}"
put $P-notifications-task poll \
  "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":[\"sqs:ReceiveMessage\",\"sqs:DeleteMessage\",\"sqs:ChangeMessageVisibility\"],\"Resource\":\"$QA:$P-notifications.fifo\"}]}"
put $P-order-processor-role tier3-read \
  "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":\"dynamodb:GetItem\",\"Resource\":\"$DA/$P-orders*\"}]}"

log "done in $(( $(date +%s) - T0 ))s"
