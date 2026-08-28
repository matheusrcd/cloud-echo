#!/usr/bin/env bash
# Probe 05 — Lambda + event source mapping.
#
# Decides whether M3's Lambda support is a thin wrapper (good) or needs a
# runtime of our own (bad). The event source mapping is the important half: a
# Tier-1 linker edge is worthless if we cannot recreate it locally.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

section "05 · lambda + event source mapping"

need zip

# --- role --------------------------------------------------------------------
cat > "$WORK_DIR/lambda-trust.json" <<'EOF'
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {"Service": "lambda.amazonaws.com"},
    "Action": "sts:AssumeRole"
  }]
}
EOF
awsx iam create-role --role-name ce-lambda-role \
  --assume-role-policy-document "file://$WORK_DIR/lambda-trust.json" >/dev/null 2>&1

# --- package -----------------------------------------------------------------
FN_DIR="$WORK_DIR/fn"
mkdir -p "$FN_DIR"
cat > "$FN_DIR/index.mjs" <<'EOF'
export const handler = async (event) => {
  const records = event.Records?.length ?? 0;
  console.log("CE_LAMBDA_INVOKED records=" + records);
  console.log("CE_LAMBDA_ENV table=" + (process.env.TABLE_NAME ?? "unset"));
  console.log("CE_LAMBDA_CREDS=" + (process.env.AWS_ACCESS_KEY_ID ? "present" : "absent"));
  return {
    statusCode: 200,
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ ok: true, records }),
  };
};
EOF
( cd "$FN_DIR" && zip -q -r "$WORK_DIR/fn.zip" index.mjs )

# --- create ------------------------------------------------------------------
if out=$(awsx lambda create-function \
    --function-name ce-fn \
    --runtime nodejs20.x \
    --handler index.handler \
    --role "arn:aws:iam::${ACCOUNT_ID}:role/ce-lambda-role" \
    --environment 'Variables={TABLE_NAME=ce-orders}' \
    --timeout 30 --memory-size 512 \
    --zip-file "fileb://$WORK_DIR/fn.zip" \
    --output json 2>&1); then
  record PASS lambda.create "nodejs20.x from zip"
  save "lambda-create.json" "$out"
else
  record FAIL lambda.create "$(last_line "$out")"
  exit 0
fi

info "waiting for function to become Active (up to 120s — first run pulls a runtime image)..."
fn_active() {
  [ "$(awsx lambda get-function-configuration --function-name ce-fn \
       --query State --output text 2>/dev/null)" = "Active" ]
}
wait_for 120 fn_active \
  && record PASS lambda.active "reached Active" \
  || record PARTIAL lambda.active "never reported Active (may still be invocable)"

# --- direct invoke -----------------------------------------------------------
if out=$(awsx lambda invoke --function-name ce-fn \
    --cli-binary-format raw-in-base64-out \
    --payload '{"ping":true}' \
    --log-type Tail \
    "$WORK_DIR/lambda-out.json" --output json 2>&1); then
  body=$(cat "$WORK_DIR/lambda-out.json" 2>/dev/null)
  save "lambda-invoke.json" "$out"
  # The handler's payload is a JSON string nested inside `body`, so it arrives
  # escaped as \"ok\". Match on the escaped form too.
  if printf '%s' "$body" | grep -qE '\\?"ok\\?"'; then
    record PASS lambda.invoke "handler executed, returned $(last_line "$body")"
  else
    record PARTIAL lambda.invoke "invoked but unexpected payload: $(last_line "$body")"
  fi

  # Real execution environment, or a stub?
  logtail=$(printf '%s' "$out" | jq -r '.LogResult // ""' | base64 -d 2>/dev/null)
  save "lambda-logtail.txt" "$logtail"
  printf '%s' "$logtail" | grep -q CE_LAMBDA_INVOKED \
    && record PASS lambda.real-runtime "our console.log appears in LogResult" \
    || record PARTIAL lambda.real-runtime "no LogResult from our code — check $WORK_DIR/lambda-logtail.txt"

  printf '%s' "$logtail" | grep -q 'table=ce-orders' \
    && record PASS lambda.env-injection "environment variables reach the handler" \
    || record PARTIAL lambda.env-injection "TABLE_NAME not observed in the handler"
else
  record FAIL lambda.invoke "$(last_line "$out")"
fi

# --- container-image lambda --------------------------------------------------
# Real accounts use image-based Lambdas heavily; blueprint `code.mode: ecr`
# depends on this working.
if awsx ecr create-repository --repository-name ce-fn-image >/dev/null 2>&1; then
  record PASS lambda.ecr-repo "ECR repository creatable locally"
else
  record PARTIAL lambda.ecr-repo "could not create ECR repo — image-based Lambda untestable here"
fi

# --- event source mapping ----------------------------------------------------
queue_url=$(cat "$WORK_DIR/queue-url.txt" 2>/dev/null)
if [ -z "$queue_url" ]; then
  record UNKNOWN lambda.esm "probe 02 did not produce a queue; run it first"
else
  queue_arn=$(awsx sqs get-queue-attributes --queue-url "$queue_url" \
    --attribute-names QueueArn --query 'Attributes.QueueArn' --output text 2>/dev/null)

  if out=$(awsx lambda create-event-source-mapping \
      --function-name ce-fn --event-source-arn "$queue_arn" \
      --batch-size 5 --enabled --output json 2>&1); then
    record PASS lambda.esm-create "SQS $queue_arn → ce-fn"
    save "lambda-esm.json" "$out"
  else
    record FAIL lambda.esm-create "$(last_line "$out")"
  fi

  # End-to-end: does a message actually trigger the function?
  marker="ce-esm-$(date +%s)"
  awsx sqs send-message --queue-url "$queue_url" \
    --message-body "{\"marker\":\"$marker\"}" >/dev/null 2>&1

  info "waiting up to 60s for the ESM to drain the queue..."
  drained() {
    [ "$(awsx sqs get-queue-attributes --queue-url "$queue_url" \
         --attribute-names ApproximateNumberOfMessages \
         --query 'Attributes.ApproximateNumberOfMessages' --output text 2>/dev/null)" = "0" ]
  }
  if wait_for 60 drained; then
    record PASS lambda.esm-delivery "queue drained — the mapping is live"
  else
    record FAIL lambda.esm-delivery "message still queued after 60s — mapping is not polling"
  fi
fi

# --- cloudwatch logs ---------------------------------------------------------
# M3 wants `cloud-echo logs lambda/x` to work; check the log group exists.
if out=$(awsx logs describe-log-streams --log-group-name /aws/lambda/ce-fn \
    --output json 2>&1); then
  n=$(printf '%s' "$out" | jq -r '.logStreams | length')
  record PASS lambda.logs "$n log stream(s) under /aws/lambda/ce-fn"
else
  record PARTIAL lambda.logs "no log group — 'cloud-echo logs' must fall back to docker logs"
fi

save "lambda-arn.txt" "arn:aws:lambda:${AWS_REGION}:${ACCOUNT_ID}:function:ce-fn"
