#!/usr/bin/env bash
# SNS validation topology, and what links to it.
#
#   ce-test-orders-topic   standard; delivers to
#     sqs ce-test-orders-events      raw delivery, a filter policy (the queue's
#                                    policy lets the topic send)
#     lambda ce-test-audit-writer    undeliverable messages → ce-test-orders-events-dlq
#     https → the ALB (16-aws-elbv2-create.sh), with basic auth and a token
#                                    in the URL: never confirmed, so pending
#   ce-test-alerts         standard; CloudWatch may publish to it (topic policy);
#     sqs ce-test-inbox              whose queue has no policy: SNS cannot deliver
#   ce-test-notifications.fifo   FIFO, named like the FIFO queue it delivers to
#
# Plus a Lambda permission for ce-test-alerts on ce-test-webhook-receiver, which
# no subscription uses (stale), and the Tier-2/3 inputs:
#   orders-fn        ORDERS_TOPIC_ARN (the topic's ARN) + sns:Publish on it
#   order-processor  ALERTS_TOPIC=ce-test-alerts (a bare name), no grant
#   webhook-receiver NOTIFY_TOPIC=ce-test-notifications.fifo — a topic's name
#                    and a queue's
#
# Costs nothing idle: topics and subscriptions are free; the few deliveries SNS
# attempts are inside the free tier. The planted basic-auth password and token
# are random per run, kept in .work/ (gitignored) for check-sns.py.
source "$(dirname "$0")/lib.sh"
T0=$(date +%s)
must() { "$@" || { echo "  FAILED: $*" | cut -c1-160 | redact >&2; exit 1; }; }
R=$AWS_REGION
TAGS="Key=$TAG_KEY,Value=$TAG_VAL"
QARN="arn:aws:sqs:$R:$ACCT"
qurl() { aws sqs get-queue-url --queue-name "$1" --query QueueUrl --output text; }

# allow_topic <queue> <topic-arn>: adds a statement letting the topic send to
# the queue, keeping whatever the queue's policy already says.
allow_topic() {
  local url; url=$(qurl "$1")
  aws sqs get-queue-attributes --queue-url "$url" --attribute-names Policy --query Attributes.Policy --output text > "$WORK/qpolicy-$1.json"
  python3 - "$WORK/qpolicy-$1.json" "$QARN:$1" "$2" <<'PY'
import json, sys
path, queue, topic = sys.argv[1:]
raw = open(path).read().strip()
doc = json.loads(raw) if raw not in ("", "None") else {"Version": "2012-10-17", "Statement": []}
sid = "sns-" + topic.rsplit(":", 1)[1].replace(".", "-")
doc["Statement"] = [s for s in doc["Statement"] if s.get("Sid") != sid] + [{
    "Sid": sid, "Effect": "Allow", "Principal": {"Service": "sns.amazonaws.com"},
    "Action": "sqs:SendMessage", "Resource": queue, "Condition": {"ArnEquals": {"aws:SourceArn": topic}}}]
json.dump({"Policy": json.dumps(doc)}, open(path, "w"))
PY
  must aws sqs set-queue-attributes --queue-url "$url" --attributes "file://$WORK/qpolicy-$1.json"
  echo "  $1's policy lets ${2##*:} send"
}

log "topics"
T_ORDERS=$(must aws sns create-topic --name $P-orders-topic --tags "$TAGS" --query TopicArn --output text)
T_ALERTS=$(must aws sns create-topic --name $P-alerts --tags "$TAGS" --query TopicArn --output text)
T_FIFO=$(must aws sns create-topic --name $P-notifications.fifo --attributes FifoTopic=true,ContentBasedDeduplication=true \
  --tags "$TAGS" --query TopicArn --output text)
echo "  $P-orders-topic $P-alerts $P-notifications.fifo"

# ce-test-alerts: CloudWatch alarms may publish — a publisher outside the graph.
# Written over the default policy, whose own statement is kept.
aws sns get-topic-attributes --topic-arn "$T_ALERTS" --query Attributes.Policy --output text > "$WORK/tpolicy-alerts.json"
python3 - "$WORK/tpolicy-alerts.json" "$T_ALERTS" <<'PY'
import json, sys
path, topic = sys.argv[1:]
doc = json.loads(open(path).read())
doc["Statement"] = [s for s in doc["Statement"] if s.get("Sid") != "alarms"] + [{
    "Sid": "alarms", "Effect": "Allow", "Principal": {"Service": "cloudwatch.amazonaws.com"},
    "Action": "sns:Publish", "Resource": topic}]
open(path, "w").write(json.dumps(doc))
PY
must aws sns set-topic-attributes --topic-arn "$T_ALERTS" --attribute-name Policy --attribute-value "file://$WORK/tpolicy-alerts.json"
echo "  $P-alerts: cloudwatch.amazonaws.com may publish"

# subscribe <topic> <protocol> <endpoint> [attributes-json]: idempotent — SNS
# returns the existing subscription for the same topic, protocol and endpoint.
subscribe() {
  local args=(sns subscribe --topic-arn "$1" --protocol "$2" --notification-endpoint "$3" --return-subscription-arn)
  [ -n "${4:-}" ] && args+=(--attributes "$4")
  must aws "${args[@]}" --query SubscriptionArn --output text >/dev/null
  local shown="${3##*:}"  # an ARN's last segment…
  [[ "$3" == *://* ]] && shown=$(sed -E 's#//[^@/]*@#//…@#; s#\?.*#?…#' <<<"$3")  # …or a URL, credentials and query masked
  echo "  ${1##*:} → $2 $shown"
}

log "subscriptions"
allow_topic $P-orders-events "$T_ORDERS"
subscribe "$T_ORDERS" sqs "$QARN:$P-orders-events" \
  '{"RawMessageDelivery":"true","FilterPolicy":"{\"type\":[\"order.created\",\"order.paid\"]}"}'

FN_AUDIT=$(aws lambda get-function --function-name $P-audit-writer --query Configuration.FunctionArn --output text)
try aws lambda add-permission --function-name $P-audit-writer --statement-id "sns-$P-orders-topic" --action lambda:InvokeFunction \
  --principal sns.amazonaws.com --source-arn "$T_ORDERS" >/dev/null
allow_topic $P-orders-events-dlq "$T_ORDERS"
subscribe "$T_ORDERS" lambda "$FN_AUDIT" "{\"RedrivePolicy\":\"{\\\"deadLetterTargetArn\\\":\\\"$QARN:$P-orders-events-dlq\\\"}\"}"

ALB_DNS=$(aws elbv2 describe-load-balancers --names $P-web --query 'LoadBalancers[0].DNSName' --output text)
PLANTED="$WORK/sns-planted.json"
[ -s "$PLANTED" ] || python3 -c 'import json,secrets; json.dump({"password": "pw-" + secrets.token_hex(6), "token": "tk-" + secrets.token_hex(6)}, open("'"$PLANTED"'", "w"))'
PW=$(python3 -c 'import json; print(json.load(open("'"$PLANTED"'"))["password"])')
TK=$(python3 -c 'import json; print(json.load(open("'"$PLANTED"'"))["token"])')
# HTTPS: SNS refuses inline credentials over http. The ALB has no :443
# listener, so the confirmation never arrives and the subscription stays pending.
subscribe "$T_ORDERS" https "https://$P-hook:$PW@$ALB_DNS/sns/orders?token=$TK"

# No allow_topic for ce-test-inbox: its queue has no policy, so every delivery fails.
subscribe "$T_ALERTS" sqs "$QARN:$P-inbox"
allow_topic $P-notifications.fifo "$T_FIFO"
subscribe "$T_FIFO" sqs "$QARN:$P-notifications.fifo" '{"RawMessageDelivery":"true"}'

log "a permission nothing uses: ce-test-alerts may invoke webhook-receiver"
try aws lambda add-permission --function-name $P-webhook-receiver --statement-id "sns-$P-alerts" --action lambda:InvokeFunction \
  --principal sns.amazonaws.com --source-arn "$T_ALERTS" >/dev/null
echo "  (no subscription)"

log "Tier-3 input: orders-fn may publish to ce-test-orders-topic"
must aws iam put-role-policy --role-name $P-apigw-fn-role --policy-name tier3-publish --policy-document \
  "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":\"sns:Publish\",\"Resource\":\"$T_ORDERS\"}]}"
echo "  $P-apigw-fn-role: sns:Publish on $P-orders-topic"

# setenv merges variables into a function's environment instead of replacing it.
setenv() {
  local fn=$1; shift
  aws lambda get-function-configuration --function-name "$fn" --query 'Environment.Variables' --output json > "$WORK/env-$fn.json"
  python3 - "$WORK/env-$fn.json" "$@" <<'PY'
import json, sys
path, pairs = sys.argv[1], sys.argv[2:]
env = json.load(open(path)) or {}
for p in pairs:
    k, v = p.split("=", 1)
    env[k] = v
json.dump({"Variables": env}, open(path, "w"))
PY
  must aws lambda update-function-configuration --function-name "$fn" --environment "file://$WORK/env-$fn.json" >/dev/null
  aws lambda wait function-updated-v2 --function-name "$fn"
  echo "  $fn: $(printf '%s ' "$@" | sed -E 's/=[^ ]*/=…/g')"
}

log "Tier-2 inputs: configuration naming the topics"
setenv $P-orders-fn "ORDERS_TOPIC_ARN=$T_ORDERS"
setenv $P-order-processor "ALERTS_TOPIC=$P-alerts"
setenv $P-webhook-receiver "NOTIFY_TOPIC=$P-notifications.fifo"

log "one message through each topic (free tier), so deliveries are exercised"
aws sns publish --topic-arn "$T_ORDERS" --message '{"id":"ce-test-1"}' \
  --message-attributes '{"type":{"DataType":"String","StringValue":"order.created"}}' --query MessageId --output text | sed 's/^/  orders-topic /'
aws sns publish --topic-arn "$T_ALERTS" --message 'ce-test alert' --query MessageId --output text | sed 's/^/  alerts /'

log "done in $(( $(date +%s) - T0 ))s"
