#!/usr/bin/env bash
# ELBv2 validation topology, and what links to it.
#
#   ce-test-web   internet-facing ALB, listener HTTP:80
#                   default      → ce-test-web-tg (ip) ← ECS service ce-test-web
#                   /fn/*        → ce-test-fn-tg (lambda) → ce-test-orders-fn
#                   /old/*       → redirect
#                   /health      → fixed response
#   ce-test-tcp   internal NLB, listener TCP:6379 → ce-test-tcp-tg (ip, no targets)
#   ce-test-vpclink  an API Gateway v2 VPC link: the HTTP API's ANY /web/{proxy+}
#                    goes through it to the ALB's listener
#
# COSTS MONEY — load balancers bill by the hour (~US$0.05/h for the two).
# The ECS service runs 0 tasks. Tear down with 90-aws-teardown.sh.
#
# Then the linking inputs (merged into each function's environment):
#   orders-fn        WEB_URL        → http:// the ALB (on the request path)
#   order-processor  TCP_BACKEND    → the NLB, host:port
#   webhook-receiver LEGACY_LB_URL  → an ALB called ce-test-web with another
#                                     address: the namesake trap, in a DNS name
source "$(dirname "$0")/lib.sh"
T0=$(date +%s)
must() { "$@" || { echo "  FAILED: $*" | cut -c1-160 | redact >&2; exit 1; }; }
R=$AWS_REGION
TAGS="Key=$TAG_KEY,Value=$TAG_VAL"
VPC=$(aws ec2 describe-vpcs --filters Name=is-default,Values=true --query 'Vpcs[0].VpcId' --output text)
SUBNETS=$(aws ec2 describe-subnets --filters Name=vpc-id,Values="$VPC" Name=default-for-az,Values=true --query 'Subnets[0:3].SubnetId' --output text)
SG=$(aws ec2 describe-security-groups --filters Name=vpc-id,Values="$VPC" Name=group-name,Values=default --query 'SecurityGroups[0].GroupId' --output text)
FN_ARN=$(aws lambda get-function --function-name $P-orders-fn --query Configuration.FunctionArn --output text)

# ELBv2 creates are idempotent: the same name and settings return what exists.
log "ALB ce-test-web (internet-facing)"
ALB=$(must aws elbv2 create-load-balancer --name $P-web --type application --scheme internet-facing \
  --subnets $SUBNETS --security-groups "$SG" --tags "$TAGS" --query 'LoadBalancers[0].LoadBalancerArn' --output text)
WEB_TG=$(must aws elbv2 create-target-group --name $P-web-tg --protocol HTTP --port 8080 --vpc-id "$VPC" \
  --target-type ip --health-check-path /health --tags "$TAGS" --query 'TargetGroups[0].TargetGroupArn' --output text)
FN_TG=$(must aws elbv2 create-target-group --name $P-fn-tg --target-type lambda --tags "$TAGS" \
  --query 'TargetGroups[0].TargetGroupArn' --output text)
try aws lambda add-permission --function-name $P-orders-fn --statement-id "elb-$P-fn-tg" --action lambda:InvokeFunction \
  --principal elasticloadbalancing.amazonaws.com --source-arn "$FN_TG" >/dev/null
must aws elbv2 register-targets --target-group-arn "$FN_TG" --targets "Id=$FN_ARN" >/dev/null
LISTENER=$(must aws elbv2 create-listener --load-balancer-arn "$ALB" --protocol HTTP --port 80 \
  --default-actions "Type=forward,TargetGroupArn=$WEB_TG" --tags "$TAGS" --query 'Listeners[0].ListenerArn' --output text)
rule() {  # rule <priority> <conditions> <actions>: skipped if the priority is taken
  aws elbv2 describe-rules --listener-arn "$LISTENER" --query "Rules[?Priority=='$1'] | length(@)" --output text | grep -q '^1$' && return 0
  must aws elbv2 create-rule --listener-arn "$LISTENER" --priority "$1" --conditions "$2" --actions "$3" >/dev/null
}
rule 10 'Field=path-pattern,Values=/fn/*' "Type=forward,TargetGroupArn=$FN_TG"
rule 20 'Field=path-pattern,Values=/old/*' 'Type=redirect,RedirectConfig={Protocol=HTTPS,Port=443,StatusCode=HTTP_301}'
rule 30 'Field=path-pattern,Values=/health' 'Type=fixed-response,FixedResponseConfig={StatusCode=200,ContentType=text/plain,MessageBody=ok}'
echo "  listener :80 with 3 rules"

log "ECS service ce-test-web behind it (0 tasks)"
if ! aws ecs describe-services --cluster $P-batch --services $P-web --query 'services[?status==`ACTIVE`] | length(@)' --output text | grep -q '^1$'; then
  must aws ecs create-service --cluster $P-batch --service-name $P-web --task-definition $P-orders-api:3 --desired-count 0 \
    --launch-type FARGATE --network-configuration "awsvpcConfiguration={subnets=[$(tr '\t ' ',,' <<<"$SUBNETS")],securityGroups=[$SG],assignPublicIp=ENABLED}" \
    --load-balancers "targetGroupArn=$WEB_TG,containerName=app,containerPort=8080" --tags "key=$TAG_KEY,value=$TAG_VAL" >/dev/null
fi
echo "  $P-batch/$P-web"

log "NLB ce-test-tcp (internal)"
NLB=$(must aws elbv2 create-load-balancer --name $P-tcp --type network --scheme internal --subnets $SUBNETS \
  --tags "$TAGS" --query 'LoadBalancers[0].LoadBalancerArn' --output text)
TCP_TG=$(must aws elbv2 create-target-group --name $P-tcp-tg --protocol TCP --port 6379 --vpc-id "$VPC" \
  --target-type ip --tags "$TAGS" --query 'TargetGroups[0].TargetGroupArn' --output text)
must aws elbv2 create-listener --load-balancer-arn "$NLB" --protocol TCP --port 6379 \
  --default-actions "Type=forward,TargetGroupArn=$TCP_TG" --tags "$TAGS" >/dev/null
echo "  listener TCP:6379"

log "API Gateway v2 VPC link → the ALB's listener"
HTTP=$(cat "$WORK/http-api-id")
VL=$(aws apigatewayv2 get-vpc-links --query "Items[?Name=='$P-vpclink'].VpcLinkId | [0]" --output text)
if [ "$VL" = "None" ] || [ -z "$VL" ]; then
  VL=$(must aws apigatewayv2 create-vpc-link --name $P-vpclink --subnet-ids $SUBNETS --security-group-ids "$SG" \
    --tags "$TAG_KEY=$TAG_VAL" --query VpcLinkId --output text)
fi
for i in $(seq 1 40); do
  st=$(aws apigatewayv2 get-vpc-link --vpc-link-id "$VL" --query VpcLinkStatus --output text)
  [ "$st" = "AVAILABLE" ] && break; sleep 15
done
[ "$st" = "AVAILABLE" ] || { echo "  FAILED: VPC link is $st" >&2; exit 1; }
if ! aws apigatewayv2 get-routes --api-id "$HTTP" --query "Items[?RouteKey=='ANY /web/{proxy+}'] | length(@)" --output text | grep -q '^1$'; then
  INT=$(must aws apigatewayv2 create-integration --api-id "$HTTP" --integration-type HTTP_PROXY --integration-method ANY \
    --connection-type VPC_LINK --connection-id "$VL" --integration-uri "$LISTENER" --payload-format-version 1.0 \
    --query IntegrationId --output text)
  must aws apigatewayv2 create-route --api-id "$HTTP" --route-key 'ANY /web/{proxy+}' --target "integrations/$INT" >/dev/null
fi
echo "  $P-vpclink AVAILABLE; ANY /web/{proxy+} → listener :80"

log "waiting for both load balancers to be active"
must aws elbv2 wait load-balancer-available --load-balancer-arns "$ALB" "$NLB"
ALB_DNS=$(aws elbv2 describe-load-balancers --load-balancer-arns "$ALB" --query 'LoadBalancers[0].DNSName' --output text)
NLB_DNS=$(aws elbv2 describe-load-balancers --load-balancer-arns "$NLB" --query 'LoadBalancers[0].DNSName' --output text)

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

log "Tier-2 inputs: configuration naming the load balancers"
setenv $P-orders-fn "WEB_URL=http://$ALB_DNS/"
setenv $P-order-processor "TCP_BACKEND=$NLB_DNS:6379"
setenv $P-webhook-receiver "LEGACY_LB_URL=http://$P-web-0000000000.$R.elb.amazonaws.com/"

log "done in $(( $(date +%s) - T0 ))s"
