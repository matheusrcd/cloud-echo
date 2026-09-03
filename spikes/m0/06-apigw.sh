#!/usr/bin/env bash
# Probe 06 — API Gateway v2.
#
# Two questions:
#   1. can we build an HTTP API → Lambda route?
#   2. WHAT IS THE INVOKE URL? Floci documents no answer, and docs/05 step 9
#      ("start the API Gateway edge listener") depends on knowing it. If the URL
#      is awkward, cloud-echo fronts it with its own listener on a clean port.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

section "06 · api gateway v2"

fn_arn=$(cat "$WORK_DIR/lambda-arn.txt" 2>/dev/null)
if [ -z "$fn_arn" ]; then
  record UNKNOWN apigw.setup "probe 05 did not produce a Lambda; run it first"
  exit 0
fi

api_id=$(awsx apigatewayv2 create-api --name ce-api --protocol-type HTTP \
  --query ApiId --output text 2>/dev/null)
if [ -n "$api_id" ] && [ "$api_id" != "None" ]; then
  record PASS apigw.create-api "$api_id"
else
  record FAIL apigw.create-api "could not create HTTP API"
  exit 0
fi

int_id=$(awsx apigatewayv2 create-integration --api-id "$api_id" \
  --integration-type AWS_PROXY \
  --integration-uri "$fn_arn" \
  --payload-format-version 2.0 \
  --query IntegrationId --output text 2>/dev/null)
[ -n "$int_id" ] && [ "$int_id" != "None" ] \
  && record PASS apigw.create-integration "AWS_PROXY → ce-fn ($int_id)" \
  || record FAIL apigw.create-integration "failed"

awsx apigatewayv2 create-route --api-id "$api_id" \
  --route-key 'POST /orders' --target "integrations/$int_id" >/dev/null 2>&1 \
  && record PASS apigw.create-route "POST /orders" \
  || record FAIL apigw.create-route "failed"

awsx apigatewayv2 create-stage --api-id "$api_id" \
  --stage-name '$default' --auto-deploy >/dev/null 2>&1 \
  && record PASS apigw.create-stage "\$default (auto-deploy)" \
  || record PARTIAL apigw.create-stage "stage creation failed or already exists"

# Lambda must permit invocation from API Gateway. In real AWS this is required;
# if Floci does not enforce it, that is itself worth knowing.
awsx lambda add-permission --function-name ce-fn \
  --statement-id ce-apigw --action lambda:InvokeFunction \
  --principal apigateway.amazonaws.com >/dev/null 2>&1

api_desc=$(awsx apigatewayv2 get-api --api-id "$api_id" --output json 2>&1)
save "apigw-api.json" "$api_desc"
advertised=$(printf '%s' "$api_desc" | jq -r '.ApiEndpoint // ""')
info "ApiEndpoint reported by Floci: ${advertised:-<none>}"

# --- find the URL that actually works ---------------------------------------
# Try every plausible shape, including the LocalStack-compatible ones, and
# record the winner. This is the concrete deliverable of this probe.
candidates=(
  "http://localhost:${FLOCI_PORT}/_aws/execute-api/${api_id}/orders"
  "http://localhost:${FLOCI_PORT}/restapis/${api_id}/\$default/_user_request_/orders"
  "http://${api_id}.execute-api.localhost:${FLOCI_PORT}/orders"
  "http://localhost:${FLOCI_PORT}/${api_id}/orders"
  "http://localhost:${FLOCI_PORT}/orders"
)
[ -n "$advertised" ] && candidates=("${advertised}/orders" "${candidates[@]}")

working=""
for url in "${candidates[@]}"; do
  code=$(curl -s -o "$WORK_DIR/apigw-body.txt" -w '%{http_code}' \
    -X POST -H 'content-type: application/json' -d '{"item":"x"}' \
    --max-time 20 "$url" 2>/dev/null)
  info "  $code  $url"
  if [ "$code" = "200" ]; then
    working="$url"
    break
  fi
done

if [ -n "$working" ]; then
  record PASS apigw.invoke "200 via $working"
  save "apigw-invoke-url.txt" "$working"
  save "apigw-response.txt" "$(cat "$WORK_DIR/apigw-body.txt")"
  grep -q '"ok"' "$WORK_DIR/apigw-body.txt" 2>/dev/null \
    && record PASS apigw.lambda-proxy "response came from the Lambda handler" \
    || record PARTIAL apigw.lambda-proxy "200 but body is not the handler's output"
else
  record FAIL apigw.invoke "no candidate URL returned 200 — cloud-echo must front API GW with its own listener"
fi

# --- REST (v1) ---------------------------------------------------------------
# Plenty of real accounts still run v1; the discovery collector covers both.
v1_id=$(awsx apigateway create-rest-api --name ce-rest --query id --output text 2>/dev/null)
[ -n "$v1_id" ] && [ "$v1_id" != "None" ] \
  && record PASS apigw.v1-create "REST API $v1_id creatable" \
  || record PARTIAL apigw.v1-create "REST (v1) API creation failed"
