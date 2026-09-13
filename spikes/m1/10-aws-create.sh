#!/usr/bin/env bash
# Creates the M1 validation topology in a real AWS account.
#
# Cost: effectively zero while idle. Nothing here runs compute: ECS services have
# desiredCount 0, Lambda functions are never invoked, DynamoDB is on-demand or at
# the 1/1 provisioned free-tier floor, SQS and IAM are free at this scale. RDS and
# ElastiCache are deliberately absent — they bill by the hour.
#
# Re-runnable: every create tolerates "already exists".
source "$(dirname "$0")/lib.sh"
T0=$(date +%s)
TAGS_KV="Key=$TAG_KEY,Value=$TAG_VAL"
ARN_BASE="arn:aws:iam::$ACCT"
Q="https://sqs.$AWS_REGION.amazonaws.com/$ACCT"
QA="arn:aws:sqs:$AWS_REGION:$ACCT"
DA="arn:aws:dynamodb:$AWS_REGION:$ACCT:table"

# ------------------------------------------------------------------ IAM policies
log "IAM customer-managed policies"
cat > "$WORK/boundary.json" <<EOF
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["dynamodb:*","sqs:*","logs:*","xray:*","cloudwatch:PutMetricData"],"Resource":"*"}]}
EOF
# A value with both '+' and a space: IAM returns this URL-encoded, and the
# collector's decoding must bring both back.
cat > "$WORK/observability.json" <<EOF
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"xray:PutTraceSegments","Resource":"*","Condition":{"StringLike":{"aws:PrincipalTag/team":"orders+payments team"}}}]}
EOF
try aws iam create-policy --policy-name $P-boundary --policy-document "file://$WORK/boundary.json" --tags "$TAGS_KV"
try aws iam create-policy --policy-name $P-observability --policy-document "file://$WORK/observability.json" --tags "$TAGS_KV"

# ------------------------------------------------------------------ IAM roles
log "IAM roles"
trust() { echo "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Principal\":{\"Service\":\"$1\"},\"Action\":\"sts:AssumeRole\"}]}"; }

# Task role under an IAM path: the role name is unique regardless of path, so
# the path must not reach the inventory id.
try aws iam create-role --path /ce-test/ --role-name $P-orders-api-task \
  --assume-role-policy-document "$(trust ecs-tasks.amazonaws.com)" \
  --permissions-boundary "$ARN_BASE:policy/$P-boundary" --tags "$TAGS_KV" Key=team,Value=orders
try aws iam put-role-policy --role-name $P-orders-api-task --policy-name orders-data --policy-document "$(cat <<EOF
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["dynamodb:GetItem","dynamodb:PutItem","dynamodb:UpdateItem","dynamodb:Query"],"Resource":["$DA/$P-orders","$DA/$P-orders/index/*"]},
 {"Effect":"Allow","Action":"sqs:SendMessage","Resource":"$QA:$P-orders-events"},
 {"Effect":"Allow","Action":"cloudwatch:PutMetricData","Resource":"*"}]}
EOF
)"
try aws iam attach-role-policy --role-name $P-orders-api-task --policy-arn "$ARN_BASE:policy/$P-observability"

try aws iam create-role --role-name $P-notifications-task --assume-role-policy-document "$(trust ecs-tasks.amazonaws.com)" --tags "$TAGS_KV"
try aws iam attach-role-policy --role-name $P-notifications-task --policy-arn arn:aws:iam::aws:policy/AmazonSQSFullAccess

# The ECS *execution* role. cloud-echo must never collect this one.
try aws iam create-role --role-name $P-ecs-exec-role --assume-role-policy-document "$(trust ecs-tasks.amazonaws.com)" --tags "$TAGS_KV"
try aws iam attach-role-policy --role-name $P-ecs-exec-role --policy-arn arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy

LBASIC=arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole
try aws iam create-role --role-name $P-order-processor-role --assume-role-policy-document "$(trust lambda.amazonaws.com)" --tags "$TAGS_KV"
try aws iam attach-role-policy --role-name $P-order-processor-role --policy-arn $LBASIC
try aws iam put-role-policy --role-name $P-order-processor-role --policy-name process --policy-document "$(cat <<EOF
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["sqs:ReceiveMessage","sqs:DeleteMessage","sqs:GetQueueAttributes"],"Resource":["$QA:$P-orders-events","$QA:$P-orders"]},
 {"Effect":"Allow","Action":"dynamodb:UpdateItem","Resource":"$DA/$P-orders"}]}
EOF
)"

try aws iam create-role --role-name $P-audit-writer-role --assume-role-policy-document "$(trust lambda.amazonaws.com)" --tags "$TAGS_KV"
try aws iam attach-role-policy --role-name $P-audit-writer-role --policy-arn $LBASIC
try aws iam put-role-policy --role-name $P-audit-writer-role --policy-name audit --policy-document "$(cat <<EOF
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["dynamodb:GetRecords","dynamodb:GetShardIterator","dynamodb:DescribeStream","dynamodb:ListStreams"],"Resource":"$DA/$P-orders/stream/*"},
 {"Effect":"Allow","Action":"dynamodb:PutItem","Resource":"$DA/$P-orders-audit"},
 {"Effect":"Allow","Action":"sqs:SendMessage","Resource":"$QA:$P-orders-events-dlq"}]}
EOF
)"

try aws iam create-role --role-name $P-webhook-receiver-role --assume-role-policy-document "$(trust lambda.amazonaws.com)" --tags "$TAGS_KV"
try aws iam attach-role-policy --role-name $P-webhook-receiver-role --policy-arn $LBASIC
try aws iam put-role-policy --role-name $P-webhook-receiver-role --policy-name enqueue --policy-document "$(cat <<EOF
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":"sqs:SendMessage","Resource":["$QA:$P-orders-events","$QA:$P-orders-events-dlq"]},
 {"Effect":"Deny","Action":"dynamodb:*","Resource":"*"}]}
EOF
)"

# Will be deleted after its function exists, leaving a dangling reference.
try aws iam create-role --path /service-role/ --role-name $P-legacy-report-role --assume-role-policy-document "$(trust lambda.amazonaws.com)" --tags "$TAGS_KV"
try aws iam attach-role-policy --role-name $P-legacy-report-role --policy-arn $LBASIC

# ------------------------------------------------------------------ SQS
log "SQS"
try aws sqs create-queue --queue-name $P-orders-events-dlq --attributes MessageRetentionPeriod=1209600 --tags "$TAG_KEY=$TAG_VAL"
cat > "$WORK/q-orders-events.json" <<EOF
{"VisibilityTimeout":"30",
 "RedrivePolicy":"{\"deadLetterTargetArn\":\"$QA:$P-orders-events-dlq\",\"maxReceiveCount\":5}",
 "Policy":"{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Principal\":{\"AWS\":\"$ARN_BASE:role/ce-test/$P-orders-api-task\"},\"Action\":\"sqs:SendMessage\",\"Resource\":\"$QA:$P-orders-events\"}]}"}
EOF
try aws sqs create-queue --queue-name $P-orders-events --attributes "file://$WORK/q-orders-events.json" --tags "$TAG_KEY=$TAG_VAL"
try aws sqs create-queue --queue-name $P-notifications.fifo --attributes FifoQueue=true,ContentBasedDeduplication=true,VisibilityTimeout=60 --tags "$TAG_KEY=$TAG_VAL"
# Same name as the DynamoDB table below: the ambiguity the linker must not guess at.
try aws sqs create-queue --queue-name $P-orders --tags "$TAG_KEY=$TAG_VAL"

# ------------------------------------------------------------------ DynamoDB
log "DynamoDB"
try aws dynamodb create-table --table-name $P-orders --billing-mode PAY_PER_REQUEST \
  --attribute-definitions AttributeName=pk,AttributeType=S AttributeName=sk,AttributeType=S AttributeName=gsi1pk,AttributeType=S AttributeName=createdAt,AttributeType=N \
  --key-schema AttributeName=pk,KeyType=HASH AttributeName=sk,KeyType=RANGE \
  --global-secondary-indexes '[{"IndexName":"gsi1","KeySchema":[{"AttributeName":"gsi1pk","KeyType":"HASH"},{"AttributeName":"createdAt","KeyType":"RANGE"}],"Projection":{"ProjectionType":"INCLUDE","NonKeyAttributes":["status","total"]}}]' \
  --stream-specification StreamEnabled=true,StreamViewType=NEW_AND_OLD_IMAGES --tags "$TAGS_KV"
try aws dynamodb create-table --table-name $P-orders-audit --billing-mode PROVISIONED --provisioned-throughput ReadCapacityUnits=1,WriteCapacityUnits=1 \
  --attribute-definitions AttributeName=id,AttributeType=S --key-schema AttributeName=id,KeyType=HASH --tags "$TAGS_KV"
aws dynamodb wait table-exists --table-name $P-orders
aws dynamodb wait table-exists --table-name $P-orders-audit
try aws dynamodb update-time-to-live --table-name $P-orders --time-to-live-specification Enabled=true,AttributeName=expiresAt
STREAM_ARN=$(aws dynamodb describe-table --table-name $P-orders --query Table.LatestStreamArn --output text)

# ------------------------------------------------------------------ Lambda
log "Lambda"
mkdir -p "$WORK/fn" && cat > "$WORK/fn/app.py" <<'PY'
def handler(event, context):
    return {"ok": True}
PY
(cd "$WORK/fn" && rm -f fn.zip && zip -q fn.zip app.py)

# New IAM roles take a few seconds before Lambda can assume them.
mkfn() {
  local name=$1 role=$2; shift 2
  for i in $(seq 1 15); do
    out=$(aws lambda create-function --function-name "$name" --runtime python3.12 --handler app.handler \
      --zip-file "fileb://$WORK/fn/fn.zip" --role "$role" --tags "$TAG_KEY=$TAG_VAL" "$@" 2>&1) && { echo "  created $name"; return 0; }
    grep -q 'ResourceConflictException' <<<"$out" && { echo "  (exists) $name"; return 0; }
    grep -qiE 'cannot be assumed|role defined for the function' <<<"$out" || { echo "$out" | redact >&2; return 1; }
    sleep 4
  done
  echo "  gave up waiting for $role" >&2; return 1
}

mkfn $P-order-processor "$ARN_BASE:role/$P-order-processor-role" --timeout 30 --memory-size 256 \
  --environment "Variables={TABLE_NAME=$P-orders,SLACK_WEBHOOK_URL=https://hooks.slack.com/services/T-FAKE/B-FAKE/Xq7Lm2Pz9Rt4Wn6Ks1Vb8Hd3,PAYMENTS_TOKEN=not-a-real-payments-token-0000,DATABASE_URL=postgres://app:not-a-real-password@$P-db.cluster-abc123.$AWS_REGION.rds.amazonaws.com:5432/orders}"
mkfn $P-audit-writer "$ARN_BASE:role/$P-audit-writer-role" --architectures arm64 --environment "Variables={AUDIT_TABLE=$P-orders-audit}"
mkfn $P-webhook-receiver "$ARN_BASE:role/$P-webhook-receiver-role" \
  --environment "Variables={ORDERS_QUEUE_URL=$Q/$P-orders-events}" --dead-letter-config "TargetArn=$QA:$P-orders-events-dlq"
mkfn $P-legacy-report "$ARN_BASE:role/service-role/$P-legacy-report-role"

for f in $P-order-processor $P-audit-writer $P-webhook-receiver $P-legacy-report; do
  aws lambda wait function-active-v2 --function-name "$f"
done

if ! aws lambda get-alias --function-name $P-order-processor --name live >/dev/null 2>&1; then
  V=$(aws lambda publish-version --function-name $P-order-processor --query Version --output text)
  try aws lambda create-alias --function-name $P-order-processor --name live --function-version "$V"
fi
try aws lambda add-permission --function-name $P-webhook-receiver --statement-id apigw-invoke \
  --action lambda:InvokeFunction --principal apigateway.amazonaws.com \
  --source-arn "arn:aws:execute-api:$AWS_REGION:$ACCT:abcdef1234/*/POST/webhooks/pay"

log "event source mappings"
try aws lambda create-event-source-mapping --function-name $P-order-processor:live --event-source-arn "$QA:$P-orders-events" \
  --batch-size 10 --maximum-batching-window-in-seconds 5 \
  --filter-criteria '{"Filters":[{"Pattern":"{\"body\":{\"type\":[\"order.created\"]}}"}]}'
try aws lambda create-event-source-mapping --function-name $P-audit-writer --event-source-arn "$STREAM_ARN" \
  --starting-position LATEST --batch-size 100 --destination-config "{\"OnFailure\":{\"Destination\":\"$QA:$P-orders-events-dlq\"}}"
try aws lambda create-event-source-mapping --function-name $P-order-processor --event-source-arn "$QA:$P-orders" --no-enabled

log "dangling role: delete legacy-report's role, keep the function"
if aws iam get-role --role-name $P-legacy-report-role >/dev/null 2>&1; then
  aws iam detach-role-policy --role-name $P-legacy-report-role --policy-arn $LBASIC
  aws iam delete-role --role-name $P-legacy-report-role && echo "  deleted $P-legacy-report-role"
fi

# ------------------------------------------------------------------ ECS
log "ECS"
VPC=$(aws ec2 describe-vpcs --filters Name=is-default,Values=true --query 'Vpcs[0].VpcId' --output text)
SUBNETS=$(aws ec2 describe-subnets --filters Name=default-for-az,Values=true --query 'Subnets[0:2].SubnetId' --output text | tr '\t' ',')
SG=$(aws ec2 describe-security-groups --filters Name=vpc-id,Values="$VPC" Name=group-name,Values=default --query 'SecurityGroups[0].GroupId' --output text)
NET="awsvpcConfiguration={subnets=[$SUBNETS],securityGroups=[$SG],assignPublicIp=DISABLED}"

try aws ecs create-cluster --cluster-name $P-main --tags key=$TAG_KEY,value=$TAG_VAL
try aws ecs create-cluster --cluster-name $P-batch --tags key=$TAG_KEY,value=$TAG_VAL

cat > "$WORK/td-orders-api.json" <<EOF
{"family":"$P-orders-api","networkMode":"awsvpc","requiresCompatibilities":["FARGATE"],"cpu":"256","memory":"512",
 "taskRoleArn":"$ARN_BASE:role/ce-test/$P-orders-api-task","executionRoleArn":"$ARN_BASE:role/$P-ecs-exec-role",
 "tags":[{"key":"$TAG_KEY","value":"$TAG_VAL"}],
 "containerDefinitions":[
  {"name":"app","image":"public.ecr.aws/nginx/nginx:1.27","essential":true,
   "portMappings":[{"containerPort":8080,"protocol":"tcp","name":"http"}],
   "environment":[
     {"name":"TABLE_NAME","value":"$P-orders"},
     {"name":"QUEUE_URL","value":"$Q/$P-orders-events"},
     {"name":"DB_HOST","value":"$P-db.cluster-abc123.$AWS_REGION.rds.amazonaws.com"},
     {"name":"DATABASE_URL","value":"postgres://orders_app:not-a-real-password@$P-db.cluster-abc123.$AWS_REGION.rds.amazonaws.com:5432/orders"},
     {"name":"PAYMENTS_URL","value":"https://api.payments.example.com"},
     {"name":"PAYMENTS_API_KEY","value":"not-a-real-payments-key-7f3a9c"},
     {"name":"LOG_LEVEL","value":"info"}],
   "secrets":[{"name":"DB_PASSWORD","valueFrom":"arn:aws:secretsmanager:$AWS_REGION:$ACCT:secret:$P/orders/db-AbCdEf"}],
   "dependsOn":[{"containerName":"otel","condition":"START"}],
   "logConfiguration":{"logDriver":"awslogs","options":{"awslogs-group":"/ecs/$P-orders-api","awslogs-region":"$AWS_REGION","awslogs-stream-prefix":"ecs"}}},
  {"name":"otel","image":"public.ecr.aws/aws-observability/aws-otel-collector:v0.41.0","essential":false}]}
EOF
cat > "$WORK/td-notifications.json" <<EOF
{"family":"$P-notifications","networkMode":"awsvpc","requiresCompatibilities":["FARGATE"],"cpu":"256","memory":"512",
 "taskRoleArn":"$ARN_BASE:role/$P-notifications-task",
 "containerDefinitions":[{"name":"worker","image":"public.ecr.aws/docker/library/node:20-alpine","essential":true,
   "command":["node","worker.js","--smtp-password=not-a-real-smtp-pass"],
   "environment":[{"name":"QUEUE_URL","value":"$Q/$P-notifications.fifo"},
                  {"name":"CACHE_HOST","value":"$P-sessions.abc123.ng.0001.use2.cache.amazonaws.com"}]}]}
EOF
aws ecs register-task-definition --cli-input-json "file://$WORK/td-orders-api.json" --query taskDefinition.taskDefinitionArn --output text | redact
aws ecs register-task-definition --cli-input-json "file://$WORK/td-notifications.json" --query taskDefinition.taskDefinitionArn --output text | redact

svc() {  # cluster name taskdef — desiredCount 0: registered, never running, free
  local out
  out=$(aws ecs create-service --cluster "$1" --service-name "$2" --task-definition "$3" --desired-count 0 \
    --launch-type FARGATE --network-configuration "$NET" --tags key=$TAG_KEY,value=$TAG_VAL 2>&1) && { echo "  created $1/$2"; return 0; }
  grep -qiE 'not idempotent|already exists' <<<"$out" && { echo "  (exists) $1/$2"; return 0; }
  echo "$out" | redact >&2; return 1
}
svc $P-main $P-orders-api $P-orders-api
svc $P-main $P-notifications $P-notifications
# Same service name in a second cluster: ids must be cluster-scoped.
svc $P-batch $P-orders-api $P-orders-api
# Eleven more services so ListServices (10 per page) really paginates and
# DescribeServices (10 per call) really batches.
for i in $(seq -w 1 11); do svc $P-main $P-filler-$i $P-notifications; done

log "done in $(( $(date +%s) - T0 ))s"
