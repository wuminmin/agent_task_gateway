#!/usr/bin/env bash
# Diagnosis-only harness for the observer statement window. NOT a Campaign, NOT
# an activation smoke, and NOT publication-eligible.
#
# It drives the same public OA-approved query_sql path a Result-heavy Artifact
# cell uses, and captures a per-queryid pg_stat_statements delta for
# gateway_reader across the same interval the old observer covered. It changes
# no capability, no activation support and no contract state; it only reads.
#
# Normalized query text is printed to the local diagnosis directory for
# inspection. Only the approved projection is committed; see the Stage B rules.
set -euo pipefail

PROJ="${DIAG_PROJECT:?set DIAG_PROJECT}"
OUT="${DIAG_OUT:?set DIAG_OUT}"
GATEWAY="${DIAG_GATEWAY:-http://127.0.0.1:8082}"
OA="${DIAG_OA:-http://127.0.0.1:8092}"
ROWS="${DIAG_ROWS:-100}"

pg() { docker exec "${PROJ}-business-postgres-1" psql -U postgres -d travel_demo "$@"; }

# One census row per (queryid, toplevel), for gateway_reader only. userid/dbid
# are resolved in-query so a role or database rename cannot silently widen it.
census() {
  pg -tAF'|' -c "
SELECT s.queryid, s.toplevel, s.calls, s.rows,
       replace(replace(s.query, e'\n', ' '), '|', ' ')
FROM pg_stat_statements s
WHERE s.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND s.userid = (SELECT oid FROM pg_roles WHERE rolname = 'gateway_reader')
ORDER BY s.queryid, s.toplevel;"
}

stats_info() {
  pg -tAF'|' -c "SELECT stats_reset, dealloc,
    current_setting('pg_stat_statements.track'),
    current_setting('pg_stat_statements.track_utility') FROM pg_stat_statements_info;"
}

mcp() { # tool, json-arguments
  curl -sS -X POST "$GATEWAY/mcp" \
    -H "Authorization: Bearer $ALICE_TOKEN" \
    -H 'Content-Type: application/json' -H 'Accept: application/json' \
    -d "$(jq -nc --arg t "$1" --argjson a "$2" \
         '{jsonrpc:"2.0",id:1,method:"tools/call",params:{name:$t,arguments:$a}}')"
}

# MCP tool results arrive as a JSON document inside content[0].text, so every
# field read below goes through this rather than through a recursive descent
# that would silently miss keys nested inside that string.
unwrap() { jq -r '.result.content[0].text' | jq -c '.'; }

csrf() { grep -o 'name="csrf"[^>]*value="[^"]*"' | head -1 | sed 's/.*value="\([^"]*\)".*/\1/'; }

oa_login() { # jar, user, password
  local jar="$1" user="$2" pass="$3" token
  token="$(curl -sS -c "$jar" "$OA/login" | csrf)"
  curl -sS -b "$jar" -c "$jar" -X POST "$OA/login" \
    --data-urlencode "csrf=$token" --data-urlencode "username=$user" \
    --data-urlencode "password=$pass" -o /dev/null
}

oa_act() { # jar, draft, action, decision
  local jar="$1" draft="$2" action="$3" decision="${4:-}" token
  token="$(curl -sS -b "$jar" -c "$jar" "$OA/tasks/$draft" | csrf)"
  if [[ -n "$decision" ]]; then
    curl -sS -b "$jar" -c "$jar" -X POST "$OA/tasks/$draft/$action" \
      --data-urlencode "csrf=$token" --data-urlencode "decision=$decision" -o /dev/null
  else
    curl -sS -b "$jar" -c "$jar" -X POST "$OA/tasks/$draft/$action" \
      --data-urlencode "csrf=$token" -o /dev/null
  fi
}

mkdir -p "$OUT/raw"
stats_info > "$OUT/raw/stats-info-before.txt"

echo "== provisioning the OA-approved Result-heavy task =="
created="$(mcp request_data_task "$(jq -nc --argjson rows "$ROWS" '{
  objective: "observer window diagnosis (not for publication)",
  data_products: ["final_v5_result_heavy"],
  columns: {final_v5_result_heavy: ["row_id","category","amount","event_date"]},
  scopes: {category: ["alpha","beta","gamma","delta"]}
}')")"
echo "$created" > "$OUT/raw/request_data_task.json"
task_id="$(unwrap <<<"$created" | jq -r '.task_id // empty')"
oa_url="$(unwrap <<<"$created" | jq -r '.oa_url // empty')"
[[ -n "$task_id" && -n "$oa_url" ]] || { echo "task request did not return an identity" >&2; exit 1; }
draft="${oa_url##*/}"
echo "task_id=$task_id draft=$draft"

alice_jar="$(mktemp)"; bob_jar="$(mktemp)"
trap 'rm -f "$alice_jar" "$bob_jar"' EXIT
oa_login "$alice_jar" alice "$OA_ALICE_PASSWORD"
oa_login "$bob_jar" bob "$OA_BOB_PASSWORD"
oa_act "$alice_jar" "$draft" submit
oa_act "$bob_jar" "$draft" decision approved

for _ in $(seq 1 60); do
  state="$(mcp get_task_status "$(jq -nc --arg id "$task_id" '{task_id:$id}')" | unwrap | jq -r '.state // empty')"
  [[ "$state" == "ACTIVE" ]] && break
  sleep 1
done
echo "task state: ${state:-unknown}"
[[ "$state" == "ACTIVE" ]] || { echo "task did not reach ACTIVE" >&2; exit 1; }

# ---- the measured window: exactly what the old observer bracketed ----
census > "$OUT/raw/census-before.txt"
stats_info > "$OUT/raw/stats-info-window-before.txt"

echo "== executing the frozen S6-x4 BDG query through public query_sql =="
# The exact contract-indexed template bytes, with the single bind parameter
# rendered. Reading the frozen file keeps the diagnosis from drifting away from
# evaluation/final-v5-wsl2/contracts/index-v1.json.
sql_text="$(sed "s/\$1/$ROWS/" evaluation/final-v5-wsl2/sql/contracts/S6-x4-bdg.sql)"
printf '%s' "$sql_text" > "$OUT/raw/rendered-query.sql"
mcp query_sql "$(jq -nc --arg id "$task_id" --arg rid "diagnosis-$(date -u +%s)" --arg sql "$sql_text" \
  '{task_id: $id, request_id: $rid, sql: $sql}')" > "$OUT/raw/query_sql.json"

census > "$OUT/raw/census-after.txt"
stats_info > "$OUT/raw/stats-info-window-after.txt"
echo "== window captured =="
