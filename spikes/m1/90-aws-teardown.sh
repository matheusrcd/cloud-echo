#!/usr/bin/env bash
# Removes everything the 1x-aws-*.sh scripts created, and the local Floci. Touches only names starting with ce-test- in the configured
# account and region, lists them first, and asks before deleting anything.
#
# Left in place on purpose: the ECS service-linked role (AWSServiceRoleForECS),
# which AWS created the first time ECS was used in the account. Other ECS usage
# depends on it, so removing it is not this script's call.
source "$(dirname "$0")/lib.sh"

fns=$(aws lambda list-functions --query "Functions[?starts_with(FunctionName,'$P-')].FunctionName" --output text)
tables=$(aws dynamodb list-tables --query "TableNames[?starts_with(@,'$P-')]" --output text)
queues=$(aws sqs list-queues --queue-name-prefix "$P-" --query QueueUrls --output text 2>/dev/null | grep -v None)
clusters="$P-main $P-batch"
roles=$(aws iam list-roles --query "Roles[?starts_with(RoleName,'$P-')].RoleName" --output text)
policies=$(aws iam list-policies --scope Local --query "Policies[?starts_with(PolicyName,'$P-')].Arn" --output text)
restapis=$(aws apigateway get-rest-apis --query "items[?starts_with(name,'$P-')].id" --output text)
httpapis=$(aws apigatewayv2 get-apis --query "Items[?starts_with(Name,'$P-')].ApiId" --output text)
apikeys=$(aws apigateway get-api-keys --query "items[?starts_with(name,'$P-')].id" --output text)
dbinstances=$(aws rds describe-db-instances --query "DBInstances[?starts_with(DBInstanceIdentifier,'$P-')].DBInstanceIdentifier" --output text)
dbclusters=$(aws rds describe-db-clusters --query "DBClusters[?starts_with(DBClusterIdentifier,'$P-')].DBClusterIdentifier" --output text)

log "will delete (account $(echo "$ACCT" | redact), region $AWS_REGION)"
printf '  lambda:   %s\n  dynamodb: %s\n  sqs:      %s\n  ecs:      %s (all services)\n  iam role: %s\n  iam pol:  %s\n  apigw:    rest=%s http=%s keys=%s\n  rds:      instances=%s clusters=%s (no final snapshot)\n  local:    ce-m1 Floci containers, network, volume\n' \
  "$fns" "$tables" "$(echo "$queues" | tr '\t' '\n' | sed 's#.*/##' | tr '\n' ' ')" "$clusters" "$roles" "$(echo "$policies" | tr '\t' '\n' | sed 's#.*/##' | tr '\n' ' ')" "$restapis" "$httpapis" "$apikeys" "$dbinstances" "$dbclusters"
if [ "${1:-}" != "--yes" ]; then
  read -r -p "type DELETE to continue: " ans; [ "$ans" = "DELETE" ] || { echo "aborted"; exit 1; }
fi

log "rds (started first: deleting a database takes minutes)"
# Instances go first — a cluster cannot be deleted while it has members — and
# with no final snapshot: these hold nothing. RDS deletes the managed master
# secrets with them.
for d in $dbinstances; do
  aws rds delete-db-instance --db-instance-identifier "$d" --skip-final-snapshot --delete-automated-backups >/dev/null && echo "  instance $d (deleting)"
done

log "api gateway"
# API Gateway throttles deletes hard (a few per minute for REST APIs); pace them.
for a in $restapis; do aws apigateway delete-rest-api --rest-api-id "$a" && echo "  rest $a"; sleep 31; done
for a in $httpapis; do aws apigatewayv2 delete-api --api-id "$a" && echo "  http $a"; done
for k in $apikeys; do aws apigateway delete-api-key --api-key "$k" && echo "  key $k"; done

log "lambda (mappings first)"
for f in $fns; do
  for u in $(aws lambda list-event-source-mappings --function-name "$f" --query 'EventSourceMappings[].UUID' --output text); do
    aws lambda delete-event-source-mapping --uuid "$u" >/dev/null && echo "  mapping $u"
  done
  for u in $(aws lambda list-event-source-mappings --function-name "$f:live" --query 'EventSourceMappings[].UUID' --output text 2>/dev/null); do
    aws lambda delete-event-source-mapping --uuid "$u" >/dev/null && echo "  mapping $u"
  done
  aws lambda delete-function --function-name "$f" && echo "  $f"
done

log "ecs"
for c in $clusters; do
  for s in $(aws ecs list-services --cluster "$c" --query serviceArns --output text 2>/dev/null); do
    aws ecs delete-service --cluster "$c" --service "$s" --force >/dev/null && echo "  ${s##*/}"
  done
  aws ecs delete-cluster --cluster "$c" >/dev/null 2>&1 && echo "  cluster $c"
done
for td in $(aws ecs list-task-definitions --family-prefix "$P-" --query taskDefinitionArns --output text); do
  aws ecs deregister-task-definition --task-definition "$td" >/dev/null && echo "  deregistered ${td##*/}"
done

log "dynamodb"
for t in $tables; do aws dynamodb delete-table --table-name "$t" >/dev/null && echo "  $t"; done

log "sqs"
for q in $queues; do aws sqs delete-queue --queue-url "$q" && echo "  ${q##*/}"; done

log "iam"
for r in $roles; do
  for a in $(aws iam list-attached-role-policies --role-name "$r" --query 'AttachedPolicies[].PolicyArn' --output text); do
    aws iam detach-role-policy --role-name "$r" --policy-arn "$a"; done
  for i in $(aws iam list-role-policies --role-name "$r" --query PolicyNames --output text); do
    aws iam delete-role-policy --role-name "$r" --policy-name "$i"; done
  aws iam delete-role-permissions-boundary --role-name "$r" 2>/dev/null
  aws iam delete-role --role-name "$r" && echo "  role $r"
done
for p in $policies; do aws iam delete-policy --policy-arn "$p" && echo "  policy ${p##*/}"; done

log "rds clusters (after their instances are gone)"
for d in $dbinstances; do aws rds wait db-instance-deleted --db-instance-identifier "$d" && echo "  instance $d deleted"; done
for c in $dbclusters; do
  aws rds delete-db-cluster --db-cluster-identifier "$c" --skip-final-snapshot >/dev/null && echo "  cluster $c (deleting)"
done
for c in $dbclusters; do aws rds wait db-cluster-deleted --db-cluster-identifier "$c" && echo "  cluster $c deleted"; done

log "local Floci"
# Only containers on this suite's network: Floci's children join it. A broader
# filter (label=floci=true) would also remove containers from any other Floci
# setup on the machine.
for id in $(docker network ls -q --filter 'name=^ce-m1$'); do
  docker ps -aq --filter "network=$id" | xargs -r docker rm -f >/dev/null 2>&1
  docker network rm "$id" >/dev/null
done
docker rm -f ce-m1-floci ce-m1-dockerproxy >/dev/null 2>&1
docker volume rm ce-m1-data >/dev/null 2>&1
echo "  done"
