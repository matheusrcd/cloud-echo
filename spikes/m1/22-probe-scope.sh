#!/usr/bin/env bash
# Proves the scanner policy's API Gateway scope with the scanner's own
# credentials: API definitions readable; API key values, usage plans, custom
# domains and client certificates refused.
source "$(dirname "$0")/lib.sh"
CREDS=$(aws sts assume-role --role-arn "arn:aws:iam::${ACCT}:role/$P-scanner" --role-session-name scope-probe \
  --query 'Credentials.[AccessKeyId,SecretAccessKey,SessionToken]' --output text) || { echo "cannot assume $P-scanner" >&2; exit 1; }
read -r AK SK ST <<<"$CREDS"
[ -n "$AK" ] && [ -n "$ST" ] || { echo "empty credentials — refusing to run with a fallback profile" >&2; exit 1; }
as_scanner() {
  env -u AWS_PROFILE AWS_ACCESS_KEY_ID="$AK" AWS_SECRET_ACCESS_KEY="$SK" AWS_SESSION_TOKEN="$ST" \
    AWS_CONFIG_FILE=/dev/null AWS_SHARED_CREDENTIALS_FILE=/dev/null AWS_REGION="$AWS_REGION" aws "$@" 2>&1
}
who=$(as_scanner sts get-caller-identity --query Arn --output text | redact)
echo "  acting as: $who"
verdict() { if grep -q AccessDenied <<<"$1"; then echo "DENIED"; elif grep -qiE 'error' <<<"$1"; then echo "ERROR: $(tail -1 <<<"$1" | cut -c1-80)"; else echo "ALLOWED"; fi; }
fails=0
expect() {  # expect ALLOWED|DENIED label cmd...
  local want=$1 label=$2; shift 2
  local got; got=$(verdict "$(as_scanner "$@")")
  [ "$got" = "$want" ] && s=PASS || { s=FAIL; fails=$((fails+1)); }
  printf "  %s  %-40s %s\n" "$s" "$label" "$got"
}
expect ALLOWED "apigateway get-rest-apis"          apigateway get-rest-apis
expect ALLOWED "apigatewayv2 get-apis"             apigatewayv2 get-apis
expect DENIED  "get-api-keys --include-values"     apigateway get-api-keys --include-values
expect DENIED  "get-usage-plans"                   apigateway get-usage-plans
expect DENIED  "get-domain-names (v1)"             apigateway get-domain-names
expect DENIED  "get-client-certificates"           apigateway get-client-certificates
expect DENIED  "get-domain-names (v2)"             apigatewayv2 get-domain-names
exit $fails
