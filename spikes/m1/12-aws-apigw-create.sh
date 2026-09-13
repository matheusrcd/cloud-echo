#!/usr/bin/env bash
# API Gateway validation topology: one REST API (v1) and one HTTP API (v2), each
# fronting a Lambda, an SQS queue, a mock and an external HTTP backend.
#
# Idle cost is zero — API Gateway bills per request, and nothing here is called.
#
# API Gateway does not enforce unique API names: create-rest-api with an existing
# name makes a second API. So this script looks each API up by name first and
# skips it if it exists, rather than relying on "already exists" errors.
source "$(dirname "$0")/lib.sh"
T0=$(date +%s)
R=$AWS_REGION
ARN_BASE="arn:aws:iam::$ACCT"
LAMBDA_ARN="arn:aws:lambda:$R:$ACCT:function"
QURL="https://sqs.$R.amazonaws.com/$ACCT"
# must aborts the script on the first failure inside an API block: a
# half-built API that reports "created" is worse than a loud stop.
must() { "$@" || { echo "  FAILED: $*" | cut -c1-160 | redact >&2; exit 1; }; }
invoke_uri() { echo "arn:aws:apigateway:$R:lambda:path/2015-03-31/functions/$1/invocations"; }
trust() { echo "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Principal\":{\"Service\":\"$1\"},\"Action\":\"sts:AssumeRole\"}]}"; }

log "dependencies: roles, queue, functions"
try aws iam create-role --role-name $P-apigw-fn-role --assume-role-policy-document "$(trust lambda.amazonaws.com)" --tags "Key=$TAG_KEY,Value=$TAG_VAL"
try aws iam attach-role-policy --role-name $P-apigw-fn-role --policy-arn arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole
# The role API Gateway assumes to call SQS directly — an application identity,
# so the IAM collector must pick it up from the integration's credentials.
try aws iam create-role --role-name $P-apigw-sqs-role --assume-role-policy-document "$(trust apigateway.amazonaws.com)" --tags "Key=$TAG_KEY,Value=$TAG_VAL"
try aws iam put-role-policy --role-name $P-apigw-sqs-role --policy-name send --policy-document \
  "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":\"sqs:SendMessage\",\"Resource\":\"arn:aws:sqs:$R:$ACCT:$P-inbox\"}]}"
try aws sqs create-queue --queue-name $P-inbox --tags "$TAG_KEY=$TAG_VAL"

mkdir -p "$WORK/fn" && printf 'def handler(event, context):\n    return {"statusCode": 200, "body": "ok"}\n' > "$WORK/fn/app.py"
(cd "$WORK/fn" && rm -f fn.zip && zip -q fn.zip app.py)
mkfn() {
  for i in $(seq 1 15); do
    out=$(aws lambda create-function --function-name "$1" --runtime python3.12 --handler app.handler \
      --zip-file "fileb://$WORK/fn/fn.zip" --role "$ARN_BASE:role/$P-apigw-fn-role" --tags "$TAG_KEY=$TAG_VAL" 2>&1) && { echo "  created $1"; return 0; }
    grep -q ResourceConflictException <<<"$out" && { echo "  (exists) $1"; return 0; }
    grep -qiE 'cannot be assumed|role defined' <<<"$out" || { echo "$out" | redact >&2; return 1; }
    sleep 4
  done; return 1
}
mkfn $P-orders-fn && mkfn $P-authorizer-fn
aws lambda wait function-active-v2 --function-name $P-orders-fn
aws lambda wait function-active-v2 --function-name $P-authorizer-fn
if ! aws lambda get-alias --function-name $P-orders-fn --name live >/dev/null 2>&1; then
  V=$(aws lambda publish-version --function-name $P-orders-fn --query Version --output text)
  try aws lambda create-alias --function-name $P-orders-fn --name live --function-version "$V"
fi

# ------------------------------------------------------------------ REST API (v1)
log "REST API"
REST=$(aws apigateway get-rest-apis --query "items[?name=='$P-orders-rest'].id | [0]" --output text)
if [ "$REST" = "None" ] || [ -z "$REST" ]; then
  cat > "$WORK/rest-policy.json" <<'EOF'
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"execute-api:Invoke","Resource":"execute-api:/*"}]}
EOF
  REST=$(aws apigateway create-rest-api --name $P-orders-rest --endpoint-configuration types=REGIONAL \
    --policy "file://$WORK/rest-policy.json" --tags "$TAG_KEY=$TAG_VAL" --query id --output text) || exit 1
  ROOT=$(aws apigateway get-resources --rest-api-id "$REST" --query "items[?path=='/'].id | [0]" --output text) || exit 1
  A="--rest-api-id $REST"

  AUTH=$(aws apigateway create-authorizer $A --name $P-token-auth --type TOKEN \
    --identity-source method.request.header.Authorization \
    --authorizer-uri "$(invoke_uri "$LAMBDA_ARN:$P-authorizer-fn")" --query id --output text) || exit 1

  ORD=$(aws apigateway create-resource $A --parent-id "$ROOT" --path-part orders --query id --output text) || exit 1
  must aws apigateway put-method $A --resource-id "$ORD" --http-method GET --authorization-type CUSTOM --authorizer-id "$AUTH" >/dev/null
  must aws apigateway put-integration $A --resource-id "$ORD" --http-method GET --type AWS_PROXY --integration-http-method POST \
    --uri "$(invoke_uri "$LAMBDA_ARN:$P-orders-fn")" >/dev/null

  # Same function through an alias: the target must keep the qualifier.
  OID=$(aws apigateway create-resource $A --parent-id "$ORD" --path-part '{id}' --query id --output text) || exit 1
  must aws apigateway put-method $A --resource-id "$OID" --http-method GET --authorization-type NONE \
    --request-parameters method.request.path.id=true >/dev/null
  must aws apigateway put-integration $A --resource-id "$OID" --http-method GET --type AWS_PROXY --integration-http-method POST \
    --uri "$(invoke_uri "$LAMBDA_ARN:$P-orders-fn:live")" >/dev/null

  # Direct SQS integration: a static upstream key in requestParameters (must be
  # redacted) and a VTL template (must not be copied into Raw).
  WH=$(aws apigateway create-resource $A --parent-id "$ROOT" --path-part webhooks --query id --output text) || exit 1
  must aws apigateway put-method $A --resource-id "$WH" --http-method POST --authorization-type NONE --api-key-required >/dev/null
  must aws apigateway put-integration $A --resource-id "$WH" --http-method POST --type AWS --integration-http-method POST \
    --uri "arn:aws:apigateway:$R:sqs:path/$ACCT/$P-inbox" --credentials "$ARN_BASE:role/$P-apigw-sqs-role" \
    --request-parameters "{\"integration.request.header.Content-Type\":\"'application/x-www-form-urlencoded'\",\"integration.request.header.x-api-key\":\"'not-a-real-upstream-key'\"}" \
    --request-templates '{"application/json":"Action=SendMessage&MessageBody=$util.urlEncode($input.body)"}' >/dev/null

  H=$(aws apigateway create-resource $A --parent-id "$ROOT" --path-part health --query id --output text) || exit 1
  must aws apigateway put-method $A --resource-id "$H" --http-method GET --authorization-type NONE >/dev/null
  must aws apigateway put-integration $A --resource-id "$H" --http-method GET --type MOCK \
    --request-templates '{"application/json":"{\"statusCode\": 200}"}' >/dev/null

  L=$(aws apigateway create-resource $A --parent-id "$ROOT" --path-part legacy --query id --output text) || exit 1
  must aws apigateway put-method $A --resource-id "$L" --http-method GET --authorization-type NONE >/dev/null
  must aws apigateway put-integration $A --resource-id "$L" --http-method GET --type HTTP_PROXY --integration-http-method GET \
    --uri https://api.payments.example.com/v1/legacy >/dev/null

  must aws apigateway create-deployment $A --stage-name prod \
    --variables backend=$P-orders-fn,DB_PASSWORD=not-a-real-password >/dev/null && echo "  created $P-orders-rest (stage prod)"
  for fn in $P-orders-fn $P-authorizer-fn; do
    try aws lambda add-permission --function-name "$fn" --statement-id "apigw-rest-$REST" --action lambda:InvokeFunction \
      --principal apigateway.amazonaws.com --source-arn "arn:aws:execute-api:$R:$ACCT:$REST/*"
  done
  try aws lambda add-permission --function-name "$P-orders-fn:live" --statement-id "apigw-rest-live-$REST" --action lambda:InvokeFunction \
    --principal apigateway.amazonaws.com --source-arn "arn:aws:execute-api:$R:$ACCT:$REST/*"
else
  echo "  (exists) $P-orders-rest"
fi

# ------------------------------------------------------------------ HTTP API (v2)
log "HTTP API"
HTTP=$(aws apigatewayv2 get-apis --query "Items[?Name=='$P-orders-http'].ApiId | [0]" --output text)
if [ "$HTTP" = "None" ] || [ -z "$HTTP" ]; then
  HTTP=$(aws apigatewayv2 create-api --name $P-orders-http --protocol-type HTTP --tags "$TAG_KEY=$TAG_VAL" --query ApiId --output text) || exit 1
  B="--api-id $HTTP"
  FNI=$(aws apigatewayv2 create-integration $B --integration-type AWS_PROXY --integration-uri "$LAMBDA_ARN:$P-orders-fn" \
    --payload-format-version 2.0 --query IntegrationId --output text) || exit 1
  SQI=$(aws apigatewayv2 create-integration $B --integration-type AWS_PROXY --integration-subtype SQS-SendMessage \
    --credentials-arn "$ARN_BASE:role/$P-apigw-sqs-role" --payload-format-version 1.0 \
    --request-parameters "{\"QueueUrl\":\"$QURL/$P-inbox\",\"MessageBody\":\"\$request.body\"}" --query IntegrationId --output text) || exit 1
  HPI=$(aws apigatewayv2 create-integration $B --integration-type HTTP_PROXY --integration-method ANY \
    --integration-uri https://api.payments.example.com --payload-format-version 1.0 --query IntegrationId --output text) || exit 1
  JWT=$(aws apigatewayv2 create-authorizer $B --name $P-jwt --authorizer-type JWT --identity-source '$request.header.Authorization' \
    --jwt-configuration Audience=ce-test-client,Issuer=https://accounts.google.com --query AuthorizerId --output text) || exit 1
  # One integration shared by two routes.
  must aws apigatewayv2 create-route $B --route-key "POST /orders" --target "integrations/$FNI" --authorization-type JWT --authorizer-id "$JWT" >/dev/null
  must aws apigatewayv2 create-route $B --route-key "GET /orders/{id}" --target "integrations/$FNI" >/dev/null
  must aws apigatewayv2 create-route $B --route-key "POST /events" --target "integrations/$SQI" >/dev/null
  must aws apigatewayv2 create-route $B --route-key '$default' --target "integrations/$HPI" >/dev/null
  must aws apigatewayv2 create-stage $B --stage-name '$default' --auto-deploy >/dev/null
  must aws apigatewayv2 create-stage $B --stage-name prod --stage-variables "API_TOKEN=not-a-real-token-123,backend=$P-orders-fn" >/dev/null
  echo "  created $P-orders-http"
  try aws lambda add-permission --function-name "$P-orders-fn" --statement-id "apigw-http-$HTTP" --action lambda:InvokeFunction \
    --principal apigateway.amazonaws.com --source-arn "arn:aws:execute-api:$R:$ACCT:$HTTP/*"
else
  echo "  (exists) $P-orders-http"
fi

echo "$REST" > "$WORK/rest-api-id"; echo "$HTTP" > "$WORK/http-api-id"
log "done in $(( $(date +%s) - T0 ))s"
