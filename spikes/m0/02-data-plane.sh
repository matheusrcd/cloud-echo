#!/usr/bin/env bash
# Probe 02 — DynamoDB and SQS: the resources the M2 seeders create first.
#
# These are expected to work; the point is to confirm the *specific shapes*
# cloud-echo's blueprint declares (GSIs, streams, redrive policies) rather than
# the trivial happy path.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

section "02 · data plane (dynamodb, sqs)"

# --- dynamodb: composite key + GSI + stream ----------------------------------
cat > "$WORK_DIR/gsi.json" <<'EOF'
[{
  "IndexName": "gsi1",
  "KeySchema": [{"AttributeName": "gsi1pk", "KeyType": "HASH"}],
  "Projection": {"ProjectionType": "ALL"}
}]
EOF

if out=$(awsx dynamodb create-table \
    --table-name ce-orders \
    --attribute-definitions \
        AttributeName=pk,AttributeType=S \
        AttributeName=sk,AttributeType=S \
        AttributeName=gsi1pk,AttributeType=S \
    --key-schema AttributeName=pk,KeyType=HASH AttributeName=sk,KeyType=RANGE \
    --billing-mode PAY_PER_REQUEST \
    --global-secondary-indexes "file://$WORK_DIR/gsi.json" \
    --stream-specification StreamEnabled=true,StreamViewType=NEW_AND_OLD_IMAGES \
    --output json 2>&1); then
  record PASS ddb.create-table "composite key + gsi + stream"
  save "ddb-table.json" "$out"
else
  record FAIL ddb.create-table "$(last_line "$out")"
fi

desc=$(awsx dynamodb describe-table --table-name ce-orders --output json 2>&1)
save "ddb-describe.json" "$desc"

stream_arn=$(printf '%s' "$desc" | jq -r '.Table.LatestStreamArn // ""')
[ -n "$stream_arn" ] \
  && record PASS ddb.stream "$stream_arn" \
  || record FAIL ddb.stream "no LatestStreamArn — Lambda stream triggers cannot be wired"

gsi_status=$(printf '%s' "$desc" | jq -r '.Table.GlobalSecondaryIndexes[0].IndexStatus // "missing"')
[ "$gsi_status" != "missing" ] \
  && record PASS ddb.gsi "gsi1 → $gsi_status" \
  || record FAIL ddb.gsi "GSI not reported by DescribeTable"

# Round-trip through the GSI: creating an index that cannot be queried is a
# silent trap for anyone testing against it.
awsx dynamodb put-item --table-name ce-orders --item \
  '{"pk":{"S":"order#1"},"sk":{"S":"meta"},"gsi1pk":{"S":"customer#7"},"total":{"N":"42"}}' \
  >/dev/null 2>&1
if out=$(awsx dynamodb query --table-name ce-orders --index-name gsi1 \
    --key-condition-expression 'gsi1pk = :c' \
    --expression-attribute-values '{":c":{"S":"customer#7"}}' \
    --output json 2>&1); then
  n=$(printf '%s' "$out" | jq -r '.Count // 0')
  [ "$n" = "1" ] \
    && record PASS ddb.gsi-query "returned 1 item" \
    || record PARTIAL ddb.gsi-query "query ran but returned $n items (index may not be populated)"
else
  record FAIL ddb.gsi-query "$(last_line "$out")"
fi

# --- sqs: dlq + redrive ------------------------------------------------------
dlq_url=$(awsx sqs create-queue --queue-name ce-orders-dlq --query QueueUrl --output text 2>/dev/null)
if [ -n "$dlq_url" ]; then
  record PASS sqs.create-dlq "$dlq_url"
else
  record FAIL sqs.create-dlq "could not create DLQ"
fi

dlq_arn=$(awsx sqs get-queue-attributes --queue-url "$dlq_url" \
  --attribute-names QueueArn --query 'Attributes.QueueArn' --output text 2>/dev/null)

jq -n --arg arn "$dlq_arn" \
  '{RedrivePolicy: ({deadLetterTargetArn: $arn, maxReceiveCount: "5"} | tostring),
    VisibilityTimeout: "30"}' > "$WORK_DIR/redrive.json"

if out=$(awsx sqs create-queue --queue-name ce-orders \
    --attributes "file://$WORK_DIR/redrive.json" --output json 2>&1); then
  record PASS sqs.create-redrive "redrive → ce-orders-dlq, maxReceiveCount 5"
  save "sqs-queue.json" "$out"
else
  record FAIL sqs.create-redrive "$(last_line "$out")"
fi

queue_url=$(awsx sqs get-queue-url --queue-name ce-orders --query QueueUrl --output text 2>/dev/null)
attrs=$(awsx sqs get-queue-attributes --queue-url "$queue_url" --attribute-names All --output json 2>&1)
save "sqs-attributes.json" "$attrs"

printf '%s' "$attrs" | jq -e '.Attributes.RedrivePolicy' >/dev/null 2>&1 \
  && record PASS sqs.redrive-readback "RedrivePolicy survives GetQueueAttributes" \
  || record FAIL sqs.redrive-readback "RedrivePolicy not returned — the linker's Tier-1 DLQ rule has no local equivalent"

# Round-trip a message.
awsx sqs send-message --queue-url "$queue_url" --message-body '{"orderId":"1"}' >/dev/null 2>&1
got=$(awsx sqs receive-message --queue-url "$queue_url" --max-number-of-messages 1 --output json 2>&1)
printf '%s' "$got" | jq -e '.Messages[0].Body' >/dev/null 2>&1 \
  && record PASS sqs.roundtrip "send → receive works" \
  || record FAIL sqs.roundtrip "$(last_line "$got")"

# --- secrets manager placeholder --------------------------------------------
# M2 seeds placeholder secrets; confirm create + read works locally.
if awsx secretsmanager create-secret --name ce/orders/db \
    --secret-string '{"username":"postgres","password":"local-only"}' >/dev/null 2>&1; then
  v=$(awsx secretsmanager get-secret-value --secret-id ce/orders/db \
      --query SecretString --output text 2>/dev/null)
  [ -n "$v" ] \
    && record PASS secrets.roundtrip "placeholder secret readable locally" \
    || record PARTIAL secrets.roundtrip "created but not readable"
else
  record FAIL secrets.create "could not create secret"
fi

save "queue-url.txt" "$queue_url"
save "stream-arn.txt" "$stream_arn"
