#!/bin/sh
set -eu

# Standalone Compose acceptance suite. A dedicated Compose project keeps its
# containers and volumes separate from a developer's normal stack.

PROJECT_NAME=${TASKBOUND_INTEGRATION_PROJECT:-taskbound-gateway-integration}
KEEP_STACK=${TASKBOUND_INTEGRATION_KEEP_STACK:-0}
GATEWAY_PORT=${TASKBOUND_INTEGRATION_GATEWAY_PORT:-18080}
OA_PORT=${TASKBOUND_INTEGRATION_OA_PORT:-18090}
CONTROL_POSTGRES_PORT=${TASKBOUND_INTEGRATION_CONTROL_POSTGRES_PORT:-25433}
POSTGRES_PORT=${TASKBOUND_INTEGRATION_POSTGRES_PORT:-25434}
if [ -n "${TASKBOUND_INTEGRATION_GATEWAY_URL:-}" ]; then
  GATEWAY_URL=$TASKBOUND_INTEGRATION_GATEWAY_URL
else
  GATEWAY_URL=http://127.0.0.1:$GATEWAY_PORT
fi

: "${TASKBOUND_ALICE_TOKEN:=alice-demo-token-change-me}"
: "${TASKBOUND_CAROL_TOKEN:=carol-demo-token-change-me}"
: "${POSTGRES_DB:=travel_demo}"
: "${POSTGRES_USER:=postgres}"
: "${POSTGRES_PASSWORD:=postgres-demo-change-me}"
: "${GATEWAY_DB_PASSWORD:=gateway-reader-demo-change-me}"
: "${GATEWAY_DATA_KEY:=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=}"
: "${GATEWAY_CONNECTOR_MAX_ROWS:=1200000}"
: "${GATEWAY_RECEIPT_KEY_ID:=gateway-integration-ed25519-v1}"
: "${GATEWAY_RECEIPT_PRIVATE_KEY:=AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=}"
: "${CONTROL_POSTGRES_DB:=taskbound_gateway}"
: "${CONTROL_POSTGRES_ADMIN_PASSWORD:=control-admin-demo-change-me}"
: "${CONTROL_DB_PASSWORD:=control-app-demo-change-me}"
: "${OA_SERVICE_TOKEN:=oa-service-token-change-me}"
: "${OA_CALLBACK_SECRET:=oa-callback-secret-change-me}"
: "${OA_RECEIPT_KEY_ID:=oa-integration-ed25519-v1}"
: "${OA_RECEIPT_PRIVATE_KEY:=nWGxne/9WmC6hEr0kuwsxERJxWl7MmkZcDusAxyuf2A=}"
: "${OA_RECEIPT_PUBLIC_KEY:=11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=}"
: "${OA_SESSION_SECRET:=oa-session-secret-change-me}"
: "${OA_ALICE_PASSWORD:=alice-demo-change-me}"
: "${OA_BOB_PASSWORD:=bob-demo-change-me}"

export TASKBOUND_ALICE_TOKEN TASKBOUND_CAROL_TOKEN
export POSTGRES_DB POSTGRES_USER POSTGRES_PASSWORD GATEWAY_DB_PASSWORD
export CONTROL_POSTGRES_DB CONTROL_POSTGRES_ADMIN_PASSWORD CONTROL_DB_PASSWORD
export CONTROL_POSTGRES_PORT POSTGRES_PORT
export GATEWAY_DATA_KEY GATEWAY_CONNECTOR_MAX_ROWS GATEWAY_RECEIPT_KEY_ID GATEWAY_RECEIPT_PRIVATE_KEY
export OA_SERVICE_TOKEN OA_CALLBACK_SECRET OA_RECEIPT_KEY_ID OA_RECEIPT_PRIVATE_KEY OA_RECEIPT_PUBLIC_KEY
export OA_SESSION_SECRET
export OA_ALICE_PASSWORD OA_BOB_PASSWORD

TMP_FILE=$(mktemp /tmp/taskbound-integration.XXXXXX)
ALICE_COOKIE=$(mktemp /tmp/taskbound-alice-cookie.XXXXXX)
BOB_COOKIE=$(mktemp /tmp/taskbound-bob-cookie.XXXXXX)
OA_PAGE=$(mktemp /tmp/taskbound-oa-page.XXXXXX)

COMPOSE_PORT_OVERRIDE="services:
  control-postgres:
    ports: !override
      - 127.0.0.1:${CONTROL_POSTGRES_PORT}:5432
  business-postgres:
    ports: !override
      - 127.0.0.1:${POSTGRES_PORT}:5432
    networks:
      - business-data
      - integration-host
  gateway:
    ports: !override
      - 127.0.0.1:${GATEWAY_PORT}:8082
    depends_on:
      control-postgres:
        condition: service_healthy
      business-postgres:
        condition: service_healthy
      oa-demo:
        condition: service_healthy
  oa-demo:
    ports: !override
      - 127.0.0.1:${OA_PORT}:8092
    environment:
      PUBLIC_OA_BASE_URL: http://127.0.0.1:${OA_PORT}
  mcp-probe:
    profiles: [integration-tools]
    build:
      context: .
      args:
        TARGET: mcp-probe
    environment:
      MCP_ENDPOINT: http://gateway:8082/mcp
      MCP_TOKEN: ${TASKBOUND_ALICE_TOKEN}
      CONTROL_POSTGRES_DSN: postgres://gateway_control:${CONTROL_DB_PASSWORD}@control-postgres:5432/${CONTROL_POSTGRES_DB}?sslmode=disable
      GATEWAY_DATA_KEY: ${GATEWAY_DATA_KEY}
    networks:
      - public-edge
      - control-plane
  test-runner:
    profiles: [integration-tools]
    build:
      context: .
      target: base
    environment:
      CONTROL_TEST_POSTGRES_DSN: postgres://postgres:${CONTROL_POSTGRES_ADMIN_PASSWORD}@control-postgres:5432/${CONTROL_POSTGRES_DB}?sslmode=disable
      BUSINESS_TEST_POSTGRES_DSN: postgres://gateway_reader:${GATEWAY_DB_PASSWORD}@business-postgres:5432/${POSTGRES_DB}?sslmode=disable
    depends_on:
      control-postgres:
        condition: service_healthy
      business-postgres:
        condition: service_healthy
    networks:
      - control-plane
      - business-data
    command: [\"go\", \"test\", \"-race\", \"./...\"]
networks:
  integration-host:"

compose() {
  printf '%s\n' "$COMPOSE_PORT_OVERRIDE" | \
    docker compose --project-name "$PROJECT_NAME" --file compose.yaml --file - "$@"
}

cleanup() {
  status=$?
  trap - EXIT INT TERM
  case "$TMP_FILE" in
    /tmp/taskbound-integration.*) rm -f "$TMP_FILE" ;;
  esac
  case "$ALICE_COOKIE" in
    /tmp/taskbound-alice-cookie.*) rm -f "$ALICE_COOKIE" ;;
  esac
  case "$BOB_COOKIE" in
    /tmp/taskbound-bob-cookie.*) rm -f "$BOB_COOKIE" ;;
  esac
  case "$OA_PAGE" in
    /tmp/taskbound-oa-page.*) rm -f "$OA_PAGE" ;;
  esac
  if [ "$KEEP_STACK" != "1" ]; then
    if ! compose down --volumes --remove-orphans >/dev/null 2>&1; then
      echo "warning: failed to remove integration Compose project $PROJECT_NAME" >&2
    fi
  fi
  exit "$status"
}
trap cleanup EXIT INT TERM

fail() {
  echo "integration test failed: $*" >&2
  exit 1
}

curl_safe() {
  command curl --connect-timeout 5 --max-time 60 "$@"
}

pass() {
  echo "ok - $*"
}

assert_contains() {
  value=$1
  expected=$2
  label=$3
  case "$value" in
    *"$expected"*) ;;
    *) fail "$label: response did not contain $expected" ;;
  esac
}

assert_not_contains() {
  value=$1
  unexpected=$2
  label=$3
  case "$value" in
    *"$unexpected"*) fail "$label: response unexpectedly contained $unexpected" ;;
    *) ;;
  esac
}

json_string() {
  value=$1
  field=$2
  printf '%s\n' "$value" | sed -n "s/.*\"$field\":\"\\([^\"]*\\)\".*/\\1/p" | tail -n 1
}

csrf_from_page() {
  sed -n 's/.*name="csrf" value="\([^"]*\)".*/\1/p' "$1" | head -n 1
}

oa_login() {
  username=$1
  password=$2
  cookie_file=$3
  curl_safe --fail --silent --show-error --cookie-jar "$cookie_file" \
    "http://127.0.0.1:$OA_PORT/login" --output "$OA_PAGE"
  csrf=$(csrf_from_page "$OA_PAGE")
  [ -n "$csrf" ] || fail "OA login page omitted CSRF token for $username"
  curl_safe --fail --silent --show-error --location \
    --cookie "$cookie_file" --cookie-jar "$cookie_file" \
    --data-urlencode "csrf=$csrf" --data-urlencode "username=$username" \
    --data-urlencode "password=$password" \
    "http://127.0.0.1:$OA_PORT/login" --output "$OA_PAGE"
  assert_contains "$(cat "$OA_PAGE")" '审批任务' "OA login for $username"
}

oa_action() {
  cookie_file=$1
  draft_id=$2
  action=$3
  decision=${4:-}
  task_url="http://127.0.0.1:$OA_PORT/tasks/$draft_id"
  curl_safe --fail --silent --show-error --cookie "$cookie_file" "$task_url" --output "$OA_PAGE"
  csrf=$(csrf_from_page "$OA_PAGE")
  [ -n "$csrf" ] || fail "OA task page omitted CSRF token for $draft_id"
  if [ "$action" = "submit" ]; then
    target="$task_url/submit"
    curl_safe --fail --silent --show-error --location --cookie "$cookie_file" --cookie-jar "$cookie_file" \
      --data-urlencode "csrf=$csrf" "$target" --output "$OA_PAGE"
  else
    target="$task_url/decision"
    curl_safe --fail --silent --show-error --location --cookie "$cookie_file" --cookie-jar "$cookie_file" \
      --data-urlencode "csrf=$csrf" --data-urlencode "decision=$decision" "$target" --output "$OA_PAGE"
  fi
}

wait_task_state() {
  task_id=$1
  expected=$2
  attempt=0
  while [ "$attempt" -lt 40 ]; do
    response=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
      "{\"jsonrpc\":\"2.0\",\"id\":90,\"method\":\"tools/call\",\"params\":{\"name\":\"get_task_status\",\"arguments\":{\"task_id\":\"$task_id\"}}}")
    case "$response" in
      *"\"state\":\"$expected\""*) printf '%s\n' "$response"; return ;;
    esac
    attempt=$((attempt + 1))
    sleep 1
  done
  fail "task $task_id did not reach $expected; last response: $response"
}

mcp_call() {
  token=$1
  payload=$2
  curl_safe --fail --silent --show-error \
    --header "Authorization: Bearer $token" \
    --header "Content-Type: application/json" \
    --header "Accept: application/json" \
    --data "$payload" \
    "$GATEWAY_URL/mcp"
}

reader_psql() {
  compose exec --no-TTY --env "PGPASSWORD=$GATEWAY_DB_PASSWORD" business-postgres \
    psql --username gateway_reader --dbname "$POSTGRES_DB" --no-psqlrc \
    --set ON_ERROR_STOP=1 "$@"
}

host_psql() {
  password=$1
  port=$2
  user=$3
  database=$4
  shift 4
  PGPASSWORD=$password docker run --rm --network host --env PGPASSWORD postgres:16-bookworm \
    psql --host 127.0.0.1 --port "$port" --username "$user" --dbname "$database" \
    --no-psqlrc --set ON_ERROR_STOP=1 "$@"
}

# Remove only this script's dedicated project so reruns start deterministically.
if ! compose down --volumes --remove-orphans >/dev/null 2>&1; then
  :
fi
compose up --build --detach --wait
for builder_service in snapshot-index-detail snapshot-index-summary; do
  builder_container=$(compose ps --all --quiet "$builder_service")
  [ -n "$builder_container" ] || fail "Compose did not create $builder_service"
  builder_environment=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$builder_container")
  case "$builder_environment" in
    *"SNAPSHOT_POSTGRES_DSN=postgres://gateway_reader:"*"@business-postgres:5432/"*) ;;
    *) fail "$builder_service does not use the gateway_reader snapshot DSN" ;;
  esac
  builder_network_mode=$(docker inspect --format '{{.HostConfig.NetworkMode}}' "$builder_container")
  [ "$builder_network_mode" = "${PROJECT_NAME}_business-data" ] ||
    fail "$builder_service network is $builder_network_mode, want only business-data"
done
pass "Snapshot builders scan Business PostgreSQL through the isolated read-only network"
gateway_container=$(compose ps --quiet gateway)
[ -n "$gateway_container" ] || fail "Compose did not create a Gateway container"
gateway_environment=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$gateway_container")
assert_contains "$gateway_environment" "GATEWAY_CONNECTOR_MAX_ROWS=$GATEWAY_CONNECTOR_MAX_ROWS" "V4 connector row ceiling"
pass "Gateway deployment admits the maximum-point provenance row count"
compose --profile integration-tools run --rm --build test-runner
pass "PostgreSQL-backed unit and race tests passed"

attempt=0
until curl_safe --fail --silent --show-error "$GATEWAY_URL/health/ready" >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 30 ]; then
    fail "gateway did not become ready"
  fi
  sleep 1
done

# Both databases must be reachable through their loopback-only host mappings.
if ! host_psql "$CONTROL_DB_PASSWORD" "$CONTROL_POSTGRES_PORT" gateway_control "$CONTROL_POSTGRES_DB" \
  --tuples-only --no-align --command 'SELECT count(*) FROM tasks' >"$TMP_FILE" 2>&1; then
  fail "control PostgreSQL was not reachable on host port $CONTROL_POSTGRES_PORT"
fi
if ! host_psql "$GATEWAY_DB_PASSWORD" "$POSTGRES_PORT" gateway_reader "$POSTGRES_DB" \
  --tuples-only --no-align --command 'SELECT count(*) FROM reporting.expense_summary' >"$TMP_FILE" 2>&1; then
  fail "business PostgreSQL was not reachable on host port $POSTGRES_PORT"
fi
if host_psql "$GATEWAY_DB_PASSWORD" "$CONTROL_POSTGRES_PORT" gateway_reader "$CONTROL_POSTGRES_DB" \
  --command 'SELECT 1' >"$TMP_FILE" 2>&1; then
  fail "business gateway_reader unexpectedly authenticated to the control PostgreSQL"
fi
if host_psql "$CONTROL_DB_PASSWORD" "$POSTGRES_PORT" gateway_control "$POSTGRES_DB" \
  --command 'SELECT 1' >"$TMP_FILE" 2>&1; then
  fail "control gateway_control unexpectedly authenticated to the business PostgreSQL"
fi
volume_names=$(docker volume ls --quiet --filter "label=com.docker.compose.project=$PROJECT_NAME")
assert_contains "$volume_names" "${PROJECT_NAME}_control-pg-data" "control PostgreSQL volume"
assert_contains "$volume_names" "${PROJECT_NAME}_business-pg-data" "business PostgreSQL volume"
assert_contains "$volume_names" "${PROJECT_NAME}_snapshot-index-artifacts" "V4 snapshot-index volume"
assert_contains "$volume_names" "${PROJECT_NAME}_gateway-encrypted-spool" "V4 encrypted spool volume"
pass "host ports, application accounts, databases, and volumes are isolated"

# MCP authentication is checked before JSON-RPC dispatch.
unauthorized_status=$(curl_safe --silent --show-error \
  --output "$TMP_FILE" --write-out '%{http_code}' \
  --header 'Authorization: Bearer definitely-wrong-token' \
  --header 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"integration-test","version":"1"}}}' \
  "$GATEWAY_URL/mcp")
[ "$unauthorized_status" = "401" ] || fail "invalid MCP token returned HTTP $unauthorized_status, want 401"
unauthorized_body=$(cat "$TMP_FILE")
assert_contains "$unauthorized_body" '"error":"unauthenticated"' "invalid MCP token"
pass "MCP initialize rejects an invalid bearer token"

initialize_response=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  '{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"integration-test","version":"1"}}}')
assert_contains "$initialize_response" '"protocolVersion":"2025-06-18"' "MCP initialize"
assert_contains "$initialize_response" '"name":"taskbound-agent-data-gateway"' "MCP initialize"
assert_contains "$initialize_response" '"version":"2.1.0"' "MCP initialize"
assert_not_contains "$initialize_response" '"error"' "MCP initialize"
pass "MCP initialize succeeds with Alice credentials"

compose --profile integration-tools run --rm --build mcp-probe
pass "official Go MCP client completed a protocol-level call against the Compose Gateway"

# Tool discovery is role-filtered, not merely guarded at invocation time.
alice_tools=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  '{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}')
assert_contains "$alice_tools" '"name":"list_data_products"' "Alice tools/list"
assert_contains "$alice_tools" '"name":"execute_plan"' "Alice tools/list"
assert_not_contains "$alice_tools" '"name":"query_data"' "Alice tools/list"
assert_contains "$alice_tools" '"name":"query_sql"' "Alice tools/list"
assert_not_contains "$alice_tools" '"name":"list_audit_events"' "Alice tools/list"
assert_not_contains "$alice_tools" '"name":"get_audit_receipt"' "Alice tools/list"
pass "Alice sees query tools and no auditor tools"

carol_tools=$(mcp_call "$TASKBOUND_CAROL_TOKEN" \
  '{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{}}')
assert_contains "$carol_tools" '"name":"list_audit_events"' "Carol tools/list"
assert_contains "$carol_tools" '"name":"get_audit_receipt"' "Carol tools/list"
assert_not_contains "$carol_tools" '"name":"list_data_products"' "Carol tools/list"
assert_not_contains "$carol_tools" '"name":"query_sql"' "Carol tools/list"
pass "Carol sees auditor tools and no query tools"

carol_forbidden=$(mcp_call "$TASKBOUND_CAROL_TOKEN" \
  '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"list_data_products","arguments":{}}}')
assert_contains "$carol_forbidden" '"isError":true' "Carol forbidden tool call"
assert_contains "$carol_forbidden" '"code":"FORBIDDEN"' "Carol forbidden tool call"
pass "Carol cannot invoke a hidden Alice tool"

# Deterministic catalog and control-plane paths expose logical names only.
products_response=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  '{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"list_data_products","arguments":{}}}')
assert_contains "$products_response" '"isError":false' "list_data_products"
assert_contains "$products_response" '"name":"expense_summary"' "list_data_products"
assert_contains "$products_response" '"name":"expense_detail"' "list_data_products"
assert_contains "$products_response" '"allowed_values":["销售部","研发部","财务部"]' "list_data_products scopes"
assert_contains "$products_response" '"trace_id":"trace_' "list_data_products"
assert_not_contains "$products_response" 'reporting.expense_' "list_data_products physical-name redaction"
pass "list_data_products hides physical view names"

tasks_response=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  '{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"list_my_tasks","arguments":{}}}')
assert_contains "$tasks_response" '"isError":false' "list_my_tasks"
pass "deterministic task listing works"

# Alice submits an explicit low-sensitivity summary request. Low sensitivity
# still requires a separate human approval by Bob.
summary_request=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  '{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"request_data_task","arguments":{"objective":"按月份分析销售部差旅报销","data_products":["expense_summary"],"columns":{"expense_summary":["month","total_amount"]},"scopes":{"department":["销售部"]}}}}')
assert_contains "$summary_request" '"isError":false' "summary task request"
assert_contains "$summary_request" '"approval_mode":"manual"' "summary task approval route"
assert_contains "$summary_request" '"exposure_profile_version":"taskgate-exposure-v4"' "summary V4 profile"
assert_contains "$summary_request" '"max_outcome_facts":10' "summary V4 outcome ceiling"
assert_contains "$summary_request" '"budget_source":"catalog_profile"' "summary Catalog budget source"
assert_contains "$summary_request" '"max_queries":10' "summary complete Catalog query budget"
summary_task=$(json_string "$summary_request" task_id)
summary_oa_url=$(json_string "$summary_request" oa_url)
[ -n "$summary_task" ] && [ -n "$summary_oa_url" ] || fail "summary request omitted task_id or oa_url"
curl_safe --fail --silent --show-error --location "$summary_oa_url" --output "$OA_PAGE"
assert_contains "$(cat "$OA_PAGE")" 'OA Demo 登录' "returned public OA URL"
summary_draft=${summary_oa_url##*/}

oa_login alice "$OA_ALICE_PASSWORD" "$ALICE_COOKIE"
oa_action "$ALICE_COOKIE" "$summary_draft" submit
wait_task_state "$summary_task" AWAITING_APPROVAL >/dev/null
summary_before=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  "{\"jsonrpc\":\"2.0\",\"id\":81,\"method\":\"tools/call\",\"params\":{\"name\":\"execute_plan\",\"arguments\":{\"task_id\":\"$summary_task\",\"request_id\":\"integration-summary-before-approval\",\"plan\":{\"product\":\"expense_summary\",\"columns\":[\"month\"]}}}}")
assert_contains "$summary_before" '"code":"TASK_NOT_ACTIVE"' "summary query before Bob approval"
oa_login bob "$OA_BOB_PASSWORD" "$BOB_COOKIE"
oa_action "$BOB_COOKIE" "$summary_draft" decision approved
summary_status=$(wait_task_state "$summary_task" ACTIVE)
assert_contains "$summary_status" '"state":"ACTIVE"' "summary human approval"
pass "Bob approval gates low-sensitivity summary task"

summary_query=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  "{\"jsonrpc\":\"2.0\",\"id\":9,\"method\":\"tools/call\",\"params\":{\"name\":\"execute_plan\",\"arguments\":{\"task_id\":\"$summary_task\",\"request_id\":\"integration-summary-plan-1\",\"plan\":{\"product\":\"expense_summary\",\"columns\":[\"month\",\"total_amount\"],\"order_by\":[{\"column\":\"month\",\"direction\":\"asc\"}]}}}}")
assert_contains "$summary_query" '"isError":false' "summary structured plan"
assert_contains "$summary_query" '"row_count":' "summary structured plan"
assert_contains "$summary_query" '"exposure":' "summary exposure settlement"
assert_contains "$summary_query" '"charged_release_facts":' "summary exposure settlement"
summary_query_id=$(json_string "$summary_query" query_id)
[ -n "$summary_query_id" ] || fail "summary query omitted query_id"
pass "approved summary task executes a structured QueryPlan"

# Restart only Gateway; OA and PostgreSQL stay up. The task, budget, encrypted
# result and audit receipt must remain available from the control PostgreSQL volume.
compose restart gateway >/dev/null
attempt=0
until curl_safe --fail --silent --show-error "$GATEWAY_URL/health/ready" >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  [ "$attempt" -lt 30 ] || fail "gateway did not become ready after restart"
  sleep 1
done
summary_after_restart=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  "{\"jsonrpc\":\"2.0\",\"id\":10,\"method\":\"tools/call\",\"params\":{\"name\":\"get_task_status\",\"arguments\":{\"task_id\":\"$summary_task\"}}}")
assert_contains "$summary_after_restart" '"state":"ACTIVE"' "persisted summary task"
budget_after_restart=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  "{\"jsonrpc\":\"2.0\",\"id\":11,\"method\":\"tools/call\",\"params\":{\"name\":\"get_budget\",\"arguments\":{\"task_id\":\"$summary_task\"}}}")
assert_contains "$budget_after_restart" '"used":{"db_ms":' "persisted budget"
assert_contains "$budget_after_restart" '"queries":1' "persisted budget usage"
stored_result=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  "{\"jsonrpc\":\"2.0\",\"id\":12,\"method\":\"tools/call\",\"params\":{\"name\":\"get_query_result\",\"arguments\":{\"task_id\":\"$summary_task\",\"query_id\":\"$summary_query_id\"}}}")
assert_contains "$stored_result" '"isError":false' "persisted encrypted result"
assert_contains "$stored_result" '"result_hash":' "persisted result receipt"
assert_contains "$stored_result" '"gateway_key_id":"gateway-integration-ed25519-v1"' "signed query receipt key"
assert_contains "$stored_result" '"version":"6"' "V4 query receipt version"
assert_contains "$stored_result" '"dictionary_set_sha256":' "V4 dictionary-set receipt binding"
assert_contains "$stored_result" '"signature":' "signed query receipt signature"
carol_receipt=$(mcp_call "$TASKBOUND_CAROL_TOKEN" \
  "{\"jsonrpc\":\"2.0\",\"id\":13,\"method\":\"tools/call\",\"params\":{\"name\":\"get_audit_receipt\",\"arguments\":{\"receipt_id\":\"$summary_query_id\"}}}")
assert_contains "$carol_receipt" '"isError":false' "persisted Carol audit receipt"
assert_contains "$carol_receipt" '"current_hash":' "persisted audit chain"
assert_not_contains "$carol_receipt" '"columns":' "Carol audit receipt raw columns"
pass "Gateway restart preserves task, budget, encrypted result, and audit evidence"

# The complete Catalog profile allows ten queries. The final allowed query is
# returned and atomically exhausts max_queries; no smaller client-selected
# budget is involved.
summary_query_index=2
while [ "$summary_query_index" -le 10 ]; do
  summary_last_query=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
    "{\"jsonrpc\":\"2.0\",\"id\":14,\"method\":\"tools/call\",\"params\":{\"name\":\"execute_plan\",\"arguments\":{\"task_id\":\"$summary_task\",\"request_id\":\"integration-summary-plan-$summary_query_index\",\"plan\":{\"product\":\"expense_summary\",\"columns\":[\"month\",\"total_amount\"],\"order_by\":[{\"column\":\"month\",\"direction\":\"asc\"}]}}}}")
  assert_contains "$summary_last_query" '"isError":false' "Catalog-budget query $summary_query_index"
  summary_query_index=$((summary_query_index + 1))
done
exhausted_status=$(wait_task_state "$summary_task" ARCHIVED)
assert_contains "$exhausted_status" '"terminal_reason":"budget_exhausted"' "budget exhaustion archive"
after_exhaustion=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  "{\"jsonrpc\":\"2.0\",\"id\":15,\"method\":\"tools/call\",\"params\":{\"name\":\"query_sql\",\"arguments\":{\"task_id\":\"$summary_task\",\"request_id\":\"integration-summary-sql-3\",\"sql\":\"SELECT month FROM expense_summary\"}}}")
assert_contains "$after_exhaustion" '"isError":true' "query after budget exhaustion"
assert_contains "$after_exhaustion" '"code":"TASK_NOT_ACTIVE"' "query after budget exhaustion"
pass "hard query budget archives the task and blocks later access"

# High-sensitivity detail requires Bob. Querying before his decision fails;
# approval makes the exact displayed snapshot queryable.
detail_request=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  '{"jsonrpc":"2.0","id":16,"method":"tools/call","params":{"name":"request_data_task","arguments":{"objective":"查询销售部员工报销明细","data_products":["expense_detail"],"columns":{"expense_detail":["receipt_no","amount"]},"scopes":{"department":["销售部"]}}}}')
assert_contains "$detail_request" '"approval_mode":"manual"' "detail approval route"
detail_task=$(json_string "$detail_request" task_id)
detail_oa_url=$(json_string "$detail_request" oa_url)
[ -n "$detail_task" ] && [ -n "$detail_oa_url" ] || fail "detail request omitted task_id or oa_url"
detail_draft=${detail_oa_url##*/}
oa_action "$ALICE_COOKIE" "$detail_draft" submit
wait_task_state "$detail_task" AWAITING_APPROVAL >/dev/null
detail_before=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  "{\"jsonrpc\":\"2.0\",\"id\":17,\"method\":\"tools/call\",\"params\":{\"name\":\"query_sql\",\"arguments\":{\"task_id\":\"$detail_task\",\"request_id\":\"integration-detail-before-approval\",\"sql\":\"SELECT receipt_no FROM expense_detail\"}}}")
assert_contains "$detail_before" '"code":"TASK_NOT_ACTIVE"' "detail query before Bob approval"

oa_action "$BOB_COOKIE" "$detail_draft" decision approved
wait_task_state "$detail_task" ACTIVE >/dev/null
detail_query=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  "{\"jsonrpc\":\"2.0\",\"id\":18,\"method\":\"tools/call\",\"params\":{\"name\":\"execute_plan\",\"arguments\":{\"task_id\":\"$detail_task\",\"request_id\":\"integration-detail-plan-1\",\"plan\":{\"product\":\"expense_detail\",\"columns\":[\"receipt_no\",\"amount\"]}}}}")
assert_contains "$detail_query" '"isError":false' "detail query after Bob approval"
assert_contains "$detail_query" '"receipt_no"' "detail query after Bob approval"
pass "Bob approval gates and then enables a high-sensitivity detail query"

# A separate Bob rejection is terminal. Repeated query attempts remain denied.
reject_request=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  '{"jsonrpc":"2.0","id":19,"method":"tools/call","params":{"name":"request_data_task","arguments":{"objective":"拒绝此销售部员工报销明细申请","data_products":["expense_detail"],"columns":{"expense_detail":["receipt_no"]},"scopes":{"department":["销售部"]}}}}')
reject_task=$(json_string "$reject_request" task_id)
reject_oa_url=$(json_string "$reject_request" oa_url)
[ -n "$reject_task" ] && [ -n "$reject_oa_url" ] || fail "reject request omitted task_id or oa_url"
reject_draft=${reject_oa_url##*/}
oa_action "$ALICE_COOKIE" "$reject_draft" submit
wait_task_state "$reject_task" AWAITING_APPROVAL >/dev/null
oa_action "$BOB_COOKIE" "$reject_draft" decision rejected
rejected_status=$(wait_task_state "$reject_task" ARCHIVED)
assert_contains "$rejected_status" '"terminal_reason":"rejected"' "Bob rejected task"
for attempt in 1 2; do
  rejected_query=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
    "{\"jsonrpc\":\"2.0\",\"id\":20,\"method\":\"tools/call\",\"params\":{\"name\":\"query_sql\",\"arguments\":{\"task_id\":\"$reject_task\",\"request_id\":\"integration-rejected-query\",\"sql\":\"SELECT receipt_no FROM expense_detail\"}}}")
  assert_contains "$rejected_query" '"code":"TASK_NOT_ACTIVE"' "rejected task query attempt $attempt"
done
pass "Bob rejection archives the task permanently"

# The official-client helper seeded a real task owned by another query
# principal in this isolated control volume. Alice must receive the same
# non-enumerating response as she would for an unknown task.
carol_raw=$(mcp_call "$TASKBOUND_CAROL_TOKEN" \
  "{\"jsonrpc\":\"2.0\",\"id\":21,\"method\":\"tools/call\",\"params\":{\"name\":\"get_query_result\",\"arguments\":{\"task_id\":\"$detail_task\",\"query_id\":\"$summary_query_id\"}}}")
assert_contains "$carol_raw" '"code":"FORBIDDEN"' "Carol raw result access"
alice_unknown=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  '{"jsonrpc":"2.0","id":22,"method":"tools/call","params":{"name":"get_task_status","arguments":{"task_id":"task-owned-by-another-subject"}}}')
assert_contains "$alice_unknown" '"code":"NOT_FOUND"' "Alice non-owned task enumeration"
pass "Alice cannot read another principal's real task and Carol remains receipt-only"

# Incomplete authorization input fails before OA. Exposure-enabled grants also
# require structured plans so exact paired-snapshot provenance is available.
invalid_request=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  '{"jsonrpc":"2.0","id":23,"method":"tools/call","params":{"name":"request_data_task","arguments":{"objective":"缺少字段和范围","data_products":["expense_summary"]}}}')
assert_contains "$invalid_request" '"code":"INVALID_REQUEST"' "explicit authorization input"
direct_response=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  "{\"jsonrpc\":\"2.0\",\"id\":24,\"method\":\"tools/call\",\"params\":{\"name\":\"query_sql\",\"arguments\":{\"task_id\":\"$detail_task\",\"request_id\":\"integration-detail-sql-2\",\"sql\":\"SELECT receipt_no, amount FROM expense_detail ORDER BY receipt_no\"}}}")
assert_contains "$direct_response" '"code":"EXPOSURE_EVIDENCE_REQUIRED"' "direct SQL exposure evidence"
detail_second_plan=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  "{\"jsonrpc\":\"2.0\",\"id\":25,\"method\":\"tools/call\",\"params\":{\"name\":\"execute_plan\",\"arguments\":{\"task_id\":\"$detail_task\",\"request_id\":\"integration-detail-plan-2\",\"plan\":{\"product\":\"expense_detail\",\"columns\":[\"receipt_no\",\"amount\"],\"order_by\":[{\"column\":\"receipt_no\",\"direction\":\"asc\"}]}}}}")
assert_contains "$detail_second_plan" '"isError":false' "second structured detail plan"
pass "incomplete authorization and provenance-free direct SQL fail closed"

# PostgreSQL's gateway role can read only published reporting views.
if ! reader_psql --tuples-only --no-align \
  --command 'SELECT count(*) FROM reporting.expense_summary' >"$TMP_FILE" 2>&1; then
  fail "gateway_reader could not read the reporting view"
fi
reporting_count=$(tr -d '[:space:]' <"$TMP_FILE")
case "$reporting_count" in
  ''|*[!0-9]*) fail "reporting view returned a non-numeric row count" ;;
esac
pass "gateway_reader can read a published reporting view"

if ! reader_psql --tuples-only --no-align --command "
  SELECT count(*)
  FROM pg_catalog.pg_class AS cls
  JOIN pg_catalog.pg_namespace AS ns ON ns.oid = cls.relnamespace
  WHERE ns.nspname = 'reporting'
    AND cls.relname IN ('expense_detail', 'expense_summary')
    AND cls.relkind = 'm'
    AND cls.relispopulated
    AND pg_catalog.pg_get_userbyid(cls.relowner) = 'taskgate_snapshot_owner'" >"$TMP_FILE" 2>&1; then
  fail "could not inspect frozen reporting publication metadata"
fi
frozen_publication_count=$(tr -d '[:space:]' <"$TMP_FILE")
[ "$frozen_publication_count" = "2" ] || fail "frozen reporting publication count is $frozen_publication_count, want 2"
pass "reporting products are populated materialized snapshots owned by a NOLOGIN role"

if ! reader_psql --tuples-only --no-align \
  --command 'SELECT count(*) FROM taskgate_ordinal.expense_summary_v1' >"$TMP_FILE" 2>&1; then
  fail "gateway_reader could not read the immutable ordinal sidecar"
fi
ordinal_count=$(tr -d '[:space:]' <"$TMP_FILE")
[ "$ordinal_count" = "10" ] || fail "ordinal sidecar row count is $ordinal_count, want 10"
pass "gateway_reader can read the Catalog-pinned ordinal sidecar"

if reader_psql --tuples-only --no-align \
  --command 'SELECT count(*) FROM legacy.employees' >"$TMP_FILE" 2>&1; then
  fail "gateway_reader unexpectedly read legacy.employees"
fi
pass "gateway_reader has no access to legacy schema objects"

if ! reader_psql --tuples-only --no-align \
  --command 'SHOW default_transaction_read_only' >"$TMP_FILE" 2>&1; then
  fail "could not inspect gateway_reader read-only setting"
fi
read_only_setting=$(tr -d '[:space:]' <"$TMP_FILE")
[ "$read_only_setting" = "on" ] || fail "gateway_reader default_transaction_read_only is $read_only_setting, want on"

# Turn off the role's defensive session default for this probe. DDL must still
# fail because gateway_reader lacks CREATE privileges, independently proving
# the database grant boundary rather than only the read-only transaction flag.
if reader_psql --command "SET default_transaction_read_only=off; CREATE TABLE public.__taskbound_write_probe(id integer)" >"$TMP_FILE" 2>&1; then
  fail "gateway_reader unexpectedly executed a write/DDL statement"
fi
if reader_psql --command "SET default_transaction_read_only=off; REFRESH MATERIALIZED VIEW reporting.expense_detail" >"$TMP_FILE" 2>&1; then
  fail "gateway_reader unexpectedly refreshed a frozen reporting publication"
fi
if reader_psql --command "SET default_transaction_read_only=off; UPDATE taskgate_ordinal.expense_detail_v1 SET row_handle=row_handle WHERE false" >"$TMP_FILE" 2>&1; then
  fail "gateway_reader unexpectedly mutated an immutable ordinal sidecar"
fi
pass "gateway_reader cannot write, refresh snapshots, or mutate ordinal sidecars"

echo "all Compose end-to-end acceptance checks passed"
