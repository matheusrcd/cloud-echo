#!/usr/bin/env bash
# Run every M0 probe in order and print a summary.
#
#   ./run-all.sh              run all probes
#   ./run-all.sh 04 05        run only those probes (Floci must already be up)
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

: > "$FINDINGS"

if [ $# -gt 0 ]; then
  probes=()
  for n in "$@"; do
    match=$(find "$SPIKE_DIR" -maxdepth 1 -name "${n}-*.sh" | head -1)
    [ -n "$match" ] && probes+=("$match") || echo "no probe matching '$n'" >&2
  done
else
  probes=("$SPIKE_DIR"/0[0-8]-*.sh)
fi

started=$SECONDS
for p in "${probes[@]}"; do
  bash "$p"
done

# ---- summary ----------------------------------------------------------------
printf '\n%s━━ summary  (%ss)%s\n\n' "$c_blu" "$(( SECONDS - started ))" "$c_reset"

for status in PASS PARTIAL FAIL UNKNOWN; do
  n=$(awk -F'\t' -v s="$status" '$1==s' "$FINDINGS" | wc -l | tr -d ' ')
  case "$status" in
    PASS) col="$c_grn" ;; FAIL) col="$c_red" ;; *) col="$c_yel" ;;
  esac
  printf '   %s%-8s %3s%s\n' "$col" "$status" "$n" "$c_reset"
done

# The probes that decide the architecture. Surface them regardless of outcome.
printf '\n%s   decision-critical probes:%s\n' "$c_dim" "$c_reset"
for id in ecs.real-container ecs.metadata-v4 ecs.task-role-creds ecs.env-injection \
          ecs.service-running floci.account-id rds.connect lambda.esm-delivery apigw.invoke; do
  line=$(awk -F'\t' -v i="$id" '$2==i {print $1"\t"$3; exit}' "$FINDINGS")
  if [ -z "$line" ]; then
    printf '     %s%-9s%s %-24s not run\n' "$c_yel" "SKIPPED" "$c_reset" "$id"
  else
    st=${line%%$'\t'*}; note=${line#*$'\t'}
    case "$st" in PASS) col="$c_grn" ;; FAIL) col="$c_red" ;; *) col="$c_yel" ;; esac
    printf '     %s%-9s%s %-24s %s\n' "$col" "$st" "$c_reset" "$id" "$note"
  fi
done

failures=$(awk -F'\t' '$1=="FAIL"' "$FINDINGS" | wc -l | tr -d ' ')
printf '\n%s   findings → %s%s\n' "$c_dim" "$FINDINGS" "$c_reset"
printf '%s   raw API output → %s%s\n' "$c_dim" "$WORK_DIR" "$c_reset"
printf '%s   now write up docs/spikes/m0-findings.md%s\n\n' "$c_dim" "$c_reset"

[ "$failures" -eq 0 ]
