#!/usr/bin/env bash
# Probe 04 — ECS. THE CRITICAL PROBE.
#
# docs/09-open-questions.md Q1. Floci's docs confirm 58 ECS operations and that
# "tasks run as real Docker containers", but say NOTHING about the task metadata
# endpoint or task role credential vending. Those two things decide whether
# `runtime: floci-ecs` can be the default in docs/05-materializer.md, or whether
# `runtime: docker` becomes the default and we own a metadata shim.
#
# Do not skip any sub-probe here. Each one maps to a design decision.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

section "04 · ECS  ⚠ highest-risk probe"

# --- task role ---------------------------------------------------------------
cat > "$WORK_DIR/ecs-trust.json" <<'EOF'
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {"Service": "ecs-tasks.amazonaws.com"},
    "Action": "sts:AssumeRole"
  }]
}
EOF

awsx iam create-role --role-name ce-probe-task \
  --assume-role-policy-document "file://$WORK_DIR/ecs-trust.json" >/dev/null 2>&1 \
  && record PASS ecs.iam-role "ce-probe-task created" \
  || record PARTIAL ecs.iam-role "create-role failed or already exists"

awsx iam put-role-policy --role-name ce-probe-task --policy-name ddb \
  --policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["dynamodb:PutItem","dynamodb:GetItem"],"Resource":"*"}]}' \
  >/dev/null 2>&1

# --- cluster -----------------------------------------------------------------
awsx ecs create-cluster --cluster-name ce-m0 >/dev/null 2>&1 \
  && record PASS ecs.create-cluster "ce-m0" \
  || record FAIL ecs.create-cluster "failed"

# --- task definition ---------------------------------------------------------
# Quoted heredoc: ${...} must reach the container's shell unexpanded.
cat > "$WORK_DIR/ecs-containers.json" <<'EOF'
[{
  "name": "probe",
  "image": "alpine:3.20",
  "essential": true,
  "entryPoint": ["/bin/sh", "-c"],
  "command": [
    "echo CE_META_URI=[${ECS_CONTAINER_METADATA_URI_V4:-unset}]; echo CE_META_URI_V3=[${ECS_CONTAINER_METADATA_URI:-unset}]; echo CE_CREDS_URI=[${AWS_CONTAINER_CREDENTIALS_RELATIVE_URI:-unset}]; echo CE_ENV_INJECTED=[${PROBE_ENV:-unset}]; echo CE_SECRET_INJECTED=[${PROBE_SECRET:-unset}]; if [ -n \"${ECS_CONTAINER_METADATA_URI_V4:-}\" ]; then wget -qO- \"$ECS_CONTAINER_METADATA_URI_V4/task\" && echo CE_META_BODY_OK || echo CE_META_BODY_FAIL; fi; if [ -n \"${AWS_CONTAINER_CREDENTIALS_RELATIVE_URI:-}\" ]; then wget -qO- \"http://169.254.170.2${AWS_CONTAINER_CREDENTIALS_RELATIVE_URI}\" && echo CE_CREDS_BODY_OK || echo CE_CREDS_BODY_FAIL; fi; sleep 600"
  ],
  "environment": [{"name": "PROBE_ENV", "value": "injected"}],
  "portMappings": [{"containerPort": 8080, "hostPort": 0, "protocol": "tcp"}]
}]
EOF

if out=$(awsx ecs register-task-definition \
    --family ce-probe \
    --network-mode bridge \
    --requires-compatibilities EC2 \
    --cpu 256 --memory 512 \
    --task-role-arn "arn:aws:iam::${ACCOUNT_ID}:role/ce-probe-task" \
    --execution-role-arn "arn:aws:iam::${ACCOUNT_ID}:role/ce-probe-task" \
    --container-definitions "file://$WORK_DIR/ecs-containers.json" \
    --output json 2>&1); then
  td_arn=$(printf '%s' "$out" | jq -r '.taskDefinition.taskDefinitionArn')
  record PASS ecs.register-taskdef "$td_arn"
  save "ecs-taskdef.json" "$out"
else
  record FAIL ecs.register-taskdef "$(last_line "$out")"
  exit 1
fi

# --- run-task: does a real container appear? --------------------------------
before=$(docker ps -aq | sort | grep . || true)
if out=$(awsx ecs run-task --cluster ce-m0 --task-definition ce-probe \
    --count 1 --launch-type EC2 --output json 2>&1); then
  task_arn=$(printf '%s' "$out" | jq -r '.tasks[0].taskArn // ""')
  record PASS ecs.run-task "$task_arn"
  save "ecs-runtask.json" "$out"
else
  record FAIL ecs.run-task "$(last_line "$out")"
  task_arn=""
fi

info "waiting for a container to appear (up to 60s)..."
new_container=""
deadline=$(( SECONDS + 60 ))
while [ "$SECONDS" -lt "$deadline" ]; do
  # Prefer the precise signal: a container running the probe image. Fall back to
  # "anything new since run-task" so we still notice if Floci renames or wraps
  # the image reference.
  new_container=$(docker ps -aq --filter 'ancestor=alpine:3.20' | head -1)
  if [ -z "$new_container" ]; then
    after=$(docker ps -aq | sort | grep . || true)
    new_container=$(comm -13 \
      <(printf '%s\n' "$before" | grep . || true) \
      <(printf '%s\n' "$after"  | grep . || true) | head -1)
  fi
  [ -n "$new_container" ] && break
  sleep 2
done

if [ -n "$new_container" ]; then
  record PASS ecs.real-container "docker container ${new_container:0:12} spawned by Floci"
  save "ecs-container-inspect.json" "$(docker inspect "$new_container")"
else
  record FAIL ecs.real-container "NO container appeared — ECS may be running in mock mode"
  record UNKNOWN ecs.metadata-v4 "cannot test: no container"
  record UNKNOWN ecs.task-role-creds "cannot test: no container"
fi

# --- the two questions that decide the default runtime -----------------------
if [ -n "$new_container" ]; then
  sleep 3
  logs=$(docker logs "$new_container" 2>&1)
  save "ecs-probe-logs.txt" "$logs"
  info "probe container logs → $WORK_DIR/ecs-probe-logs.txt"

  meta_uri=$(printf '%s' "$logs" | grep -o 'CE_META_URI=\[[^]]*\]' | head -1)
  creds_uri=$(printf '%s' "$logs" | grep -o 'CE_CREDS_URI=\[[^]]*\]' | head -1)
  env_inj=$(printf '%s' "$logs" | grep -o 'CE_ENV_INJECTED=\[[^]]*\]' | head -1)

  case "$meta_uri" in
    *unset*|"") record FAIL ecs.metadata-v4 "ECS_CONTAINER_METADATA_URI_V4 not injected" ;;
    *)          if printf '%s' "$logs" | grep -q CE_META_BODY_OK; then
                  record PASS ecs.metadata-v4 "$meta_uri and /task responded"
                else
                  record PARTIAL ecs.metadata-v4 "$meta_uri injected but /task did not respond"
                fi ;;
  esac

  case "$creds_uri" in
    *unset*|"") record FAIL ecs.task-role-creds "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI not injected" ;;
    *)          if printf '%s' "$logs" | grep -q CE_CREDS_BODY_OK; then
                  record PASS ecs.task-role-creds "$creds_uri vends credentials"
                else
                  record PARTIAL ecs.task-role-creds "$creds_uri injected but returned nothing"
                fi ;;
  esac

  case "$env_inj" in
    *injected*) record PASS ecs.env-injection "task definition environment reaches the container" ;;
    *)          record FAIL ecs.env-injection "environment[] NOT injected — blocks every \${ref:} in the blueprint" ;;
  esac

  # Which network? Workloads must reach Floci and each other.
  nets=$(docker inspect "$new_container" --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}')
  if printf '%s' "$nets" | grep -q "$FLOCI_NETWORK"; then
    record PASS ecs.container-network "on $FLOCI_NETWORK — reaches Floci and siblings by name"
  else
    record FAIL ecs.container-network "networks: [$nets] — NOT on $FLOCI_NETWORK"
  fi

  # Floci injects its own AWS_ENDPOINT_URL/AWS_ACCESS_KEY_ID. The Echo Gateway
  # (ADR-0005) depends on the task definition being able to override them.
  eff_env=$(docker inspect "$new_container" --format '{{range .Config.Env}}{{println .}}{{end}}')
  save "ecs-container-env.txt" "$eff_env"
  printf '%s' "$eff_env" | grep -q 'AWS_ENDPOINT_URL=' \
    && record PARTIAL ecs.floci-injects-endpoint "Floci pre-sets $(printf '%s' "$eff_env" | grep '^AWS_ENDPOINT_URL=') — must be overridden" \
    || record PASS ecs.floci-injects-endpoint "no pre-set endpoint; we control it entirely"

  # Port publishing: the blueprint declares host ports.
  pmap=$(docker port "$new_container" 2>/dev/null | tr '\n' ' ')
  [ -n "$pmap" ] \
    && record PASS ecs.port-mapping "$pmap" \
    || record PARTIAL ecs.port-mapping "no published ports — host access to ECS workloads needs another mechanism"
fi

# --- service lifecycle -------------------------------------------------------
if out=$(awsx ecs create-service --cluster ce-m0 --service-name ce-probe-svc \
    --task-definition ce-probe --desired-count 1 --launch-type EC2 \
    --output json 2>&1); then
  record PASS ecs.create-service "ce-probe-svc created"
  save "ecs-service.json" "$out"
else
  record FAIL ecs.create-service "$(last_line "$out")"
fi

sleep 5
svc=$(awsx ecs describe-services --cluster ce-m0 --services ce-probe-svc --output json 2>&1)
save "ecs-service-describe.json" "$svc"
running=$(printf '%s' "$svc" | jq -r '.services[0].runningCount // "?"')
desired=$(printf '%s' "$svc" | jq -r '.services[0].desiredCount // "?"')
if [ "$running" = "$desired" ] && [ "$running" != "0" ]; then
  record PASS ecs.service-running "runningCount=$running desiredCount=$desired"
else
  record PARTIAL ecs.service-running "runningCount=$running desiredCount=$desired — service may not converge"
fi

# Scaling matters: the materializer downscales desiredCount to 1 and M6 needs
# per-node restart.
if awsx ecs update-service --cluster ce-m0 --service ce-probe-svc --desired-count 0 >/dev/null 2>&1; then
  record PASS ecs.update-service "scale to 0 accepted"
else
  record PARTIAL ecs.update-service "update-service rejected — no clean per-node stop"
fi
