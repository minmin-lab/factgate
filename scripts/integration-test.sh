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
: "${MINIO_ROOT_USER:=taskgate-integration-admin}"
: "${MINIO_ROOT_PASSWORD:=taskgate-integration-minio-root-9ad4f6c2e8b1}"
: "${GATEWAY_OBJECT_STORE_ACCESS_KEY:=taskgate-integration-gateway}"
: "${GATEWAY_OBJECT_STORE_SECRET_KEY:=taskgate-integration-object-store-7c1e5a9d3f8b}"
: "${GATEWAY_DELIVERY_SIGNING_KEY:=taskgate-integration-delivery-hmac-6b2d8f4a1c9e}"
GATEWAY_PUBLIC_BASE_URL=http://127.0.0.1:$GATEWAY_PORT
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
export MINIO_ROOT_USER MINIO_ROOT_PASSWORD
export GATEWAY_OBJECT_STORE_ACCESS_KEY GATEWAY_OBJECT_STORE_SECRET_KEY
export GATEWAY_DELIVERY_SIGNING_KEY GATEWAY_PUBLIC_BASE_URL
export OA_SERVICE_TOKEN OA_CALLBACK_SECRET OA_RECEIPT_KEY_ID OA_RECEIPT_PRIVATE_KEY OA_RECEIPT_PUBLIC_KEY
export OA_SESSION_SECRET
export OA_ALICE_PASSWORD OA_BOB_PASSWORD

TMP_FILE=$(mktemp /tmp/taskbound-integration.XXXXXX)
ALICE_COOKIE=$(mktemp /tmp/taskbound-alice-cookie.XXXXXX)
BOB_COOKIE=$(mktemp /tmp/taskbound-bob-cookie.XXXXXX)
OA_PAGE=$(mktemp /tmp/taskbound-oa-page.XXXXXX)
DOWNLOAD_FILE=$(mktemp /tmp/taskbound-result-download.XXXXXX)
DOWNLOAD_HEADERS=$(mktemp /tmp/taskbound-result-headers.XXXXXX)
GO_TEST_JSON=$(mktemp /tmp/taskbound-go-test.XXXXXX)
GO_TEST_REPORT=$(mktemp /tmp/taskbound-go-test-report.XXXXXX)
# Compose reads the working tree's private .env for every variable this script
# leaves unset (an operator keyring there made the Gateway reject the OA's
# integration receipt key on 2026-08-29). An empty --env-file keeps the gate
# hermetic: only this script's defaults and the caller's environment apply.
COMPOSE_ENV_FILE=$(mktemp /tmp/taskbound-compose-env.XXXXXX)

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
    image: ${PROJECT_NAME}-mcp-probe
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
    image: ${PROJECT_NAME}-test-runner
    build:
      context: .
      target: base
    environment:
      CONTROL_TEST_POSTGRES_DSN: postgres://postgres:${CONTROL_POSTGRES_ADMIN_PASSWORD}@control-postgres:5432/${CONTROL_POSTGRES_DB}?sslmode=disable
      BUSINESS_TEST_POSTGRES_DSN: postgres://gateway_reader:${GATEWAY_DB_PASSWORD}@business-postgres:5432/${POSTGRES_DB}?sslmode=disable
      RESULT_ARTIFACT_TEST_S3_ENDPOINT: http://result-object-store:9000
      RESULT_ARTIFACT_TEST_S3_REGION: us-east-1
      RESULT_ARTIFACT_TEST_S3_BUCKET: taskgate-results
      RESULT_ARTIFACT_TEST_S3_ACCESS_KEY: ${GATEWAY_OBJECT_STORE_ACCESS_KEY}
      RESULT_ARTIFACT_TEST_S3_SECRET_KEY: ${GATEWAY_OBJECT_STORE_SECRET_KEY}
    depends_on:
      control-postgres:
        condition: service_healthy
      business-postgres:
        condition: service_healthy
      result-object-store-init:
        condition: service_completed_successfully
    networks:
      - control-plane
      - business-data
      - result-storage
    # The runner image is built with **/raw dockerignored, so the retained
    # qualification artifacts the Catalog-digest discovery looks for
    # (evaluation/final-v5-wsl2/raw/diagnosis-attestation-footprint-qualification-*/
    # snapshot-index-artifacts-full) only exist on the evidence host. Mount that
    # directory read-only so those tests run here instead of skipping.
    volumes:
      - ./evaluation/final-v5-wsl2/raw:/src/evaluation/final-v5-wsl2/raw:ro
      # The snapshot-index services compile every publication once, each in its
      # own memory cgroup, onto this shared volume. Mounting it read-only lets
      # the scale-lane full-registry tests activate the already-pinned bundles
      # (TASKGATE_GATEWAY_PREBUILT_SNAPSHOT_DIR) instead of a second in-process
      # compile, whose scale-e7 transient (~26 GiB) OOMs the shared host.
      - snapshot-index-artifacts:/var/lib/taskgate/snapshot-index:ro
    command: [\"go\", \"test\", \"-race\", \"./...\"]
networks:
  integration-host:"

compose() {
  printf '%s\n' "$COMPOSE_PORT_OVERRIDE" | \
    docker compose --project-name "$PROJECT_NAME" --env-file "$COMPOSE_ENV_FILE" --file compose.yaml --file - "$@"
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
  case "$DOWNLOAD_FILE" in
    /tmp/taskbound-result-download.*) rm -f "$DOWNLOAD_FILE" ;;
  esac
  case "$DOWNLOAD_HEADERS" in
    /tmp/taskbound-result-headers.*) rm -f "$DOWNLOAD_HEADERS" ;;
  esac
  case "$GO_TEST_JSON" in
    /tmp/taskbound-go-test.*) rm -f "$GO_TEST_JSON" ;;
  esac
  case "$GO_TEST_REPORT" in
    /tmp/taskbound-go-test-report.*) rm -f "$GO_TEST_REPORT" ;;
  esac
  if [ "$KEEP_STACK" != "1" ]; then
    if ! compose down --volumes --remove-orphans >/dev/null 2>&1; then
      echo "warning: failed to remove integration Compose project $PROJECT_NAME" >&2
    fi
  fi
  # Removed last: compose down above still needs the hermetic env file.
  case "$COMPOSE_ENV_FILE" in
    /tmp/taskbound-compose-env.*) rm -f "$COMPOSE_ENV_FILE" ;;
  esac
  exit "$status"
}
trap cleanup EXIT INT TERM

fail() {
  echo "integration test failed: $*" >&2
  # cleanup tears the project down, so keep the Gateway and OA logs in this
  # run's own output (the retained raw log) for the failure diagnosis.
  if [ -n "${PROJECT_NAME:-}" ] && [ -n "${COMPOSE_PORT_OVERRIDE:-}" ]; then
    echo "--- gateway and oa-demo logs (last 300 lines each) ---" >&2
    compose logs --no-color --tail 300 gateway oa-demo >&2 2>/dev/null || true
  fi
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

assert_structured_field_absent() {
  value=$1
  field=$2
  label=$3
  if printf '%s\n' "$value" | python3 -c '
import json
import sys

field = sys.argv[1]
try:
    structured = json.load(sys.stdin)["result"]["structuredContent"]
except (KeyError, TypeError, ValueError):
    raise SystemExit(2)
if not isinstance(structured, dict):
    raise SystemExit(2)
raise SystemExit(1 if field in structured else 0)
' "$field"; then
    return 0
  else
    status=$?
  fi
  if [ "$status" -eq 1 ]; then
    fail "$label: structured response unexpectedly contained $field"
  fi
  fail "$label: response was not a valid MCP structured result"
}

json_string() {
  value=$1
  field=$2
  printf '%s\n' "$value" | sed -n "s/.*\"$field\":\"\\([^\"]*\\)\".*/\\1/p" | tail -n 1
}

# Go's JSON encoder escapes ampersands in structuredContent. Decode only the
# URL characters emitted by url.Values; do not print the resulting capability.
json_url() {
  json_string "$1" "$2" | sed 's/\\u0026/\&/g; s/\\u003d/=/g'
}

json_single_row() {
  printf '%s\n' "$1" | sed -n 's/.*"rows":\(\[\[[^]]*]\]\).*/\1/p' | tail -n 1
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
compose --profile integration-tools build test-runner
if ! compose --profile integration-tools run --rm test-runner \
  go test -json -race -count=1 -tags=taskgate_integration ./... >"$GO_TEST_JSON"; then
  cat "$GO_TEST_JSON"
  fail "complete PostgreSQL-backed Go test suite failed"
fi
cat "$GO_TEST_JSON"
# Acceptance of the complete suite is settled by the declared-skip audit that
# already accepts the DSN-enabled suite (evaluation/cmd/final-v5-dbtest-report),
# scoped to this Compose harness. A skip is a failure unless an allowance names
# the test, the reason it printed, why this container cannot run it, and the
# milestone by which it must run elsewhere; an allowance that matches nothing is
# a failure too, and a claim of "already covered by the DSN suite record" is
# checked against docs/evidence at this exact commit rather than read as prose.
# The retained snapshot artifacts carry no allowance: on the evidence host the
# tests that need them run, discovered by Catalog digest.
command -v go >/dev/null 2>&1 || fail "go is required on the host to audit the test evidence"
runner_go_version=$(compose --profile integration-tools run --rm test-runner go version | tr -d '\r')
business_container=$(compose ps --quiet business-postgres)
business_image=$(docker inspect --format '{{.Config.Image}}' "$business_container")
business_version_num=$(docker exec "$business_container" \
  psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tAc 'show server_version_num' | tr -d '[:space:]')
repository_root=$(git rev-parse --show-toplevel)
if ! go run ./evaluation/cmd/final-v5-dbtest-report -scope compose-gate \
  -evidence-root "$repository_root" -commit "$(git rev-parse HEAD)" \
  -postgresql-image "$business_image" -postgresql-version-num "$business_version_num" \
  -go-version "$runner_go_version" -timeout "go test default" \
  -out "$GO_TEST_REPORT" <"$GO_TEST_JSON"; then
  cat "$GO_TEST_REPORT" 2>/dev/null || true
  fail "complete Go test evidence was not accepted by the declared-skip audit"
fi
cat "$GO_TEST_REPORT"
pass "complete PostgreSQL-backed unit and race tests accepted: every skip declared with a due milestone"
# The crash-window case lives on the taskgate_scale lane since 436c119 (it
# prepares an ordinal-program plan); without the tag the build simply does not
# contain it and this step reports "did not execute". Measured 2026-08-29 on
# the evidence host: 114.7 s, 7.1 GB peak RSS.
promotion_recovery_output=$(compose --profile integration-tools run --rm \
  -e TASKGATE_GATEWAY_PREBUILT_SNAPSHOT_DIR=/var/lib/taskgate/snapshot-index test-runner \
  go test -json -count=1 -tags=taskgate_scale \
  -run '^TestCanonicalCopySurvivesAvailableTransactionFailureAndRecoversExactlyOnce$' \
  ./internal/gateway)
printf '%s\n' "$promotion_recovery_output"
if ! printf '%s\n' "$promotion_recovery_output" | python3 -c '
import json
import sys

name = "TestCanonicalCopySurvivesAvailableTransactionFailureAndRecoversExactlyOnce"
events = [json.loads(line) for line in sys.stdin if line.strip().startswith("{")]
passed = any(event.get("Test") == name and event.get("Action") == "pass" for event in events)
skipped = any(event.get("Test") == name and event.get("Action") == "skip" for event in events)
raise SystemExit(0 if passed and not skipped else 1)
'; then
  fail "canonical-copy/AVAILABLE-commit crash-window test did not execute and pass"
fi
pass "canonical-copy/AVAILABLE-commit crash-window recovery passed"

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
assert_contains "$volume_names" "${PROJECT_NAME}_result-object-data" "canonical Parquet object-store volume"
pass "host ports, application accounts, databases, and PostgreSQL/object-store volumes are isolated"

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

compose --profile integration-tools build mcp-probe
compose --profile integration-tools run --rm mcp-probe
pass "official Go MCP client completed a protocol-level call against the Compose Gateway"

# Tool discovery is role-filtered, not merely guarded at invocation time.
alice_tools=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  '{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}')
assert_contains "$alice_tools" '"name":"list_data_products"' "Alice tools/list"
assert_contains "$alice_tools" '"name":"describe_data_product"' "Alice tools/list"
assert_contains "$alice_tools" '"name":"get_sql_capabilities"' "Alice tools/list"
assert_not_contains "$alice_tools" '"name":"execute_plan"' "Alice tools/list"
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
assert_contains "$summary_request" '"exposure_profile_version":"taskgate-exposure-v5"' "summary V5 profile"
# Since the reviewed routing (config/catalog.yaml approval_routes, 2026-08-16) a
# low-sensitivity summary shares the default low route, final-v5-baseline-low-v1.
assert_contains "$summary_request" '"budget_profile":"final-v5-baseline-low-v1"' "summary default low route"
assert_contains "$summary_request" '"max_outcome_facts":128' "summary V5 outcome ceiling"
assert_contains "$summary_request" '"predicate_footprint":' "summary V5 predicate limits"
assert_contains "$summary_request" '"budget_source":"catalog_profile"' "summary Catalog budget source"
assert_contains "$summary_request" '"max_queries":128' "summary complete Catalog query budget"
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
assert_contains "$summary_query" '"result_id":"' "summary object-backed result"
assert_contains "$summary_query" '"artifact_status":"AVAILABLE"' "summary object-backed result"
assert_contains "$summary_query" '"row_count":' "summary structured plan"
assert_contains "$summary_query" '"exposure":' "summary exposure settlement"
assert_contains "$summary_query" '"charged_release_facts":' "summary exposure settlement"
assert_contains "$summary_query" '"outcome_radix":' "summary V5 radix telemetry"
assert_contains "$summary_query" '"blocks_loaded":' "summary V5 radix telemetry counters"
assert_structured_field_absent "$summary_query" rows "summary metadata-only response"
assert_not_contains "$summary_query" '"object_key":' "summary object-key redaction"
summary_query_id=$(json_string "$summary_query" query_id)
[ -n "$summary_query_id" ] || fail "summary query omitted query_id"
summary_result_id=$(json_string "$summary_query" result_id)
[ -n "$summary_result_id" ] || fail "summary query omitted result_id"

summary_preview_first=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  "{\"jsonrpc\":\"2.0\",\"id\":91,\"method\":\"tools/call\",\"params\":{\"name\":\"preview_result\",\"arguments\":{\"result_id\":\"$summary_result_id\",\"offset\":0,\"limit\":1}}}")
assert_contains "$summary_preview_first" '"isError":false' "first Parquet preview page"
assert_contains "$summary_preview_first" '"offset":0' "first Parquet preview page"
assert_contains "$summary_preview_first" '"limit":1' "first Parquet preview page"
assert_contains "$summary_preview_first" '"rows":[[' "first Parquet preview page"
summary_preview_second=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  "{\"jsonrpc\":\"2.0\",\"id\":92,\"method\":\"tools/call\",\"params\":{\"name\":\"preview_result\",\"arguments\":{\"result_id\":\"$summary_result_id\",\"offset\":1,\"limit\":1}}}")
assert_contains "$summary_preview_second" '"isError":false' "second Parquet preview page"
assert_contains "$summary_preview_second" '"offset":1' "second Parquet preview page"
assert_contains "$summary_preview_second" '"limit":1' "second Parquet preview page"
first_preview_row=$(json_single_row "$summary_preview_first")
second_preview_row=$(json_single_row "$summary_preview_second")
[ -n "$first_preview_row" ] && [ -n "$second_preview_row" ] || fail "Parquet preview pages omitted their single row"
[ "$first_preview_row" != "$second_preview_row" ] || fail "Parquet preview pagination returned the same row for offsets 0 and 1"

summary_delivery=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  "{\"jsonrpc\":\"2.0\",\"id\":93,\"method\":\"tools/call\",\"params\":{\"name\":\"deliver_result\",\"arguments\":{\"result_id\":\"$summary_result_id\",\"format\":\"parquet\"}}}")
assert_contains "$summary_delivery" '"isError":false' "Parquet delivery capability"
assert_contains "$summary_delivery" '"format":"parquet"' "Parquet delivery capability"
summary_download_url=$(json_url "$summary_delivery" download_url)
[ -n "$summary_download_url" ] || fail "deliver_result omitted download_url"
curl_safe --fail --silent --show-error --dump-header "$DOWNLOAD_HEADERS" \
  --output "$DOWNLOAD_FILE" "$summary_download_url"
download_size=$(wc -c <"$DOWNLOAD_FILE" | tr -d '[:space:]')
download_content_length=$(awk '
  tolower($1) == "content-length:" { gsub("\r", "", $2); length_value = $2 }
  END { print length_value }
' "$DOWNLOAD_HEADERS")
[ -n "$download_content_length" ] || fail "Parquet delivery omitted Content-Length"
[ "$download_content_length" = "$download_size" ] ||
  fail "Parquet Content-Length $download_content_length did not match downloaded size $download_size"
parquet_prefix=$(dd if="$DOWNLOAD_FILE" bs=1 count=4 2>/dev/null)
parquet_suffix=$(tail -c 4 "$DOWNLOAD_FILE")
[ "$parquet_prefix" = "PAR1" ] && [ "$parquet_suffix" = "PAR1" ] ||
  fail "delivered file did not have Parquet PAR1 boundary magic"
pass "approved query creates an AVAILABLE canonical Parquet; preview paginates and delivery streams a complete file"

# Restart only Gateway; OA, PostgreSQL, and MinIO stay up. Control PostgreSQL
# retains metadata/audit state while the encrypted canonical Parquet remains in
# the independent object-store volume.
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
assert_contains "$stored_result" '"isError":false' "persisted object-backed result"
assert_contains "$stored_result" "\"result_id\":\"$summary_result_id\"" "persisted object-backed result"
assert_contains "$stored_result" '"artifact_status":"AVAILABLE"' "persisted object-backed result"
assert_contains "$stored_result" '"row_count":' "persisted object-backed result"
assert_structured_field_absent "$stored_result" rows "persisted metadata-only response"
assert_not_contains "$stored_result" '"object_key":' "persisted object-key redaction"
assert_contains "$stored_result" '"result_hash":' "persisted result receipt"
assert_contains "$stored_result" '"gateway_key_id":"gateway-integration-ed25519-v1"' "signed query receipt key"
# One signed receipt shape since 29ee3fc; the version lives in
# internal/queryreceipt/queryreceipt.go.
assert_contains "$stored_result" '"version":"10"' "V5 artifact query receipt version"
assert_contains "$stored_result" '"artifact_intent":' "V5 artifact intent receipt binding"
assert_contains "$stored_result" '"result_metadata_sha256":' "V5 result metadata receipt binding"
assert_contains "$stored_result" '"dictionary_set_sha256":' "V4 dictionary-set receipt binding"
assert_contains "$stored_result" '"signature":' "signed query receipt signature"
catalog_runtime_digest=$(json_string "$stored_result" catalog_digest)
case "$catalog_runtime_digest" in
  *[!0-9a-f]*|'') fail "runtime Catalog digest is not lowercase SHA-256" ;;
esac
[ "${#catalog_runtime_digest}" -eq 64 ] || fail "runtime Catalog digest is not 64 hexadecimal characters"
if [ -n "${TASKGATE_COMPOSE_EVIDENCE_RUNTIME:-}" ]; then
  printf 'catalog_runtime_digest\t%s\n' "$catalog_runtime_digest" >"$TASKGATE_COMPOSE_EVIDENCE_RUNTIME"
fi
carol_receipt=$(mcp_call "$TASKBOUND_CAROL_TOKEN" \
  "{\"jsonrpc\":\"2.0\",\"id\":13,\"method\":\"tools/call\",\"params\":{\"name\":\"get_audit_receipt\",\"arguments\":{\"receipt_id\":\"$summary_query_id\"}}}")
assert_contains "$carol_receipt" '"isError":false' "persisted Carol audit receipt"
assert_contains "$carol_receipt" '"current_hash":' "persisted audit chain"
assert_contains "$carol_receipt" '"artifact_intent_inclusion":' "artifact registration inclusion proofs"
assert_contains "$carol_receipt" '"availability_event_inclusion":' "artifact availability inclusion proof"
assert_not_contains "$carol_receipt" '"columns":' "Carol audit receipt raw columns"
summary_preview_after_restart=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  "{\"jsonrpc\":\"2.0\",\"id\":94,\"method\":\"tools/call\",\"params\":{\"name\":\"preview_result\",\"arguments\":{\"result_id\":\"$summary_result_id\",\"offset\":0,\"limit\":1}}}")
assert_contains "$summary_preview_after_restart" '"isError":false' "Parquet preview after Gateway restart"
assert_contains "$summary_preview_after_restart" '"rows":[[' "Parquet preview after Gateway restart"
assert_contains "$summary_preview_after_restart" '"offset":0' "Parquet preview after Gateway restart"
pass "Gateway restart preserves Control metadata/audit state and reads the canonical Parquet from object storage"

# final-v5-baseline-low-v1 allows 128 queries. The final allowed query is
# returned and atomically exhausts max_queries; no smaller client-selected
# budget is involved.
summary_query_index=2
while [ "$summary_query_index" -le 128 ]; do
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

# Exercise the complete V5 caller-predicate path, including duplicate removal
# and NULL preservation. A reordered/deduplicated IN list must reuse the
# committed observation without another Business PostgreSQL execution. Keep
# this evidence task separate so its novel atoms do not consume the budget of
# the detail-task scenarios below.
predicate_request=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  '{"jsonrpc":"2.0","id":180,"method":"tools/call","params":{"name":"request_data_task","arguments":{"objective":"验证销售部报销谓词足迹与语义重放","data_products":["expense_detail"],"columns":{"expense_detail":["receipt_no","amount"]},"scopes":{"department":["销售部"]}}}}')
assert_contains "$predicate_request" '"approval_mode":"manual"' "V5 caller predicate approval route"
predicate_task=$(json_string "$predicate_request" task_id)
predicate_oa_url=$(json_string "$predicate_request" oa_url)
[ -n "$predicate_task" ] && [ -n "$predicate_oa_url" ] || fail "predicate request omitted task_id or oa_url"
predicate_draft=${predicate_oa_url##*/}
oa_action "$ALICE_COOKIE" "$predicate_draft" submit
wait_task_state "$predicate_task" AWAITING_APPROVAL >/dev/null
oa_action "$BOB_COOKIE" "$predicate_draft" decision approved
wait_task_state "$predicate_task" ACTIVE >/dev/null
predicate_query=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  "{\"jsonrpc\":\"2.0\",\"id\":181,\"method\":\"tools/call\",\"params\":{\"name\":\"query_sql\",\"arguments\":{\"task_id\":\"$predicate_task\",\"request_id\":\"integration-detail-predicate-1\",\"sql\":\"SELECT receipt_no, amount FROM expense_detail WHERE amount IN (100, NULL, 100, 200) ORDER BY receipt_no\"}}}")
assert_contains "$predicate_query" '"isError":false' "V5 caller predicate query"
assert_contains "$predicate_query" '"artifact_status":"AVAILABLE"' "V5 caller predicate Parquet"
assert_contains "$predicate_query" '"raw_literal_count":4' "V5 caller predicate raw literals"
assert_contains "$predicate_query" '"unique_atom_count":3' "V5 caller predicate unique atoms"
assert_contains "$predicate_query" '"actual_predicate_atom_count":3' "V5 caller predicate actual atoms"
assert_contains "$predicate_query" '"actual_outcome_facts":4' "V5 caller predicate outcome facts"
assert_contains "$predicate_query" '"actual_composite_count":1' "V5 caller predicate composite"

predicate_replay=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  "{\"jsonrpc\":\"2.0\",\"id\":182,\"method\":\"tools/call\",\"params\":{\"name\":\"query_sql\",\"arguments\":{\"task_id\":\"$predicate_task\",\"request_id\":\"integration-detail-predicate-2\",\"sql\":\"SELECT receipt_no, amount FROM expense_detail WHERE amount IN (200, 100, NULL) ORDER BY receipt_no\"}}}")
assert_contains "$predicate_replay" '"isError":false' "V5 caller predicate semantic replay"
assert_contains "$predicate_replay" '"semantic_replay":true' "V5 caller predicate semantic replay"
assert_contains "$predicate_replay" '"charged_predicate_atom_count":0' "V5 caller predicate replay atom charge"
assert_contains "$predicate_replay" '"charged_outcome_facts":0' "V5 caller predicate replay outcome charge"
assert_not_contains "$predicate_replay" '"business_postgresql"' "V5 caller predicate replay Business PG calls"
assert_not_contains "$predicate_replay" '"provenance_postgresql"' "V5 caller predicate replay provenance PG calls"
pass "V5 semantic replay avoided Business PostgreSQL and repeated exposure charge"
pass "caller SQL lowers through V5 atomization, Parquet publication, and zero-execution semantic replay"

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

# Incomplete authorization input fails before OA. Exposure-enabled SQL is
# lowered to the same exact paired-snapshot plan path used by the advanced
# structured entry point.
invalid_request=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  '{"jsonrpc":"2.0","id":23,"method":"tools/call","params":{"name":"request_data_task","arguments":{"objective":"缺少字段和范围","data_products":["expense_summary"]}}}')
assert_contains "$invalid_request" '"code":"INVALID_REQUEST"' "explicit authorization input"
direct_response=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  "{\"jsonrpc\":\"2.0\",\"id\":24,\"method\":\"tools/call\",\"params\":{\"name\":\"query_sql\",\"arguments\":{\"task_id\":\"$detail_task\",\"request_id\":\"integration-detail-sql-2\",\"sql\":\"SELECT receipt_no, amount FROM expense_detail ORDER BY receipt_no\"}}}")
assert_contains "$direct_response" '"isError":false' "exposure SQL lowering"
assert_contains "$direct_response" '"sql_profile":"taskgate-reporting-sql-v1"' "exposure SQL profile"
assert_contains "$direct_response" '"query_plan":' "exposure SQL canonical plan"
assert_contains "$direct_response" '"plan_digest":' "exposure SQL canonical identity"
detail_second_plan=$(mcp_call "$TASKBOUND_ALICE_TOKEN" \
  "{\"jsonrpc\":\"2.0\",\"id\":25,\"method\":\"tools/call\",\"params\":{\"name\":\"execute_plan\",\"arguments\":{\"task_id\":\"$detail_task\",\"request_id\":\"integration-detail-plan-2\",\"plan\":{\"product\":\"expense_detail\",\"columns\":[\"receipt_no\",\"amount\"],\"order_by\":[{\"column\":\"receipt_no\",\"direction\":\"asc\"}]}}}}")
assert_contains "$detail_second_plan" '"isError":false' "second structured detail plan"
pass "incomplete authorization fails closed and exposure SQL uses the canonical plan path"

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

# A recording wrapper may request immutable image IDs before cleanup removes
# this run's containers. The file is temporary input to the signed-off Compose
# receipt and is never itself treated as evidence.
if [ -n "${TASKGATE_COMPOSE_EVIDENCE_IMAGES:-}" ]; then
  : >"$TASKGATE_COMPOSE_EVIDENCE_IMAGES"
  for evidence_service in control-postgres business-postgres snapshot-index-detail snapshot-index-summary \
    result-object-store result-object-store-init gateway oa-demo; do
    evidence_container=$(compose ps --all --quiet "$evidence_service")
    [ -n "$evidence_container" ] || fail "evidence container missing for $evidence_service"
    evidence_image_id=$(docker inspect --format '{{.Image}}' "$evidence_container")
    evidence_image_ref=$(docker inspect --format '{{.Config.Image}}' "$evidence_container")
    printf '%s\t%s\t%s\n' "$evidence_service" "$evidence_image_ref" "$evidence_image_id" >>"$TASKGATE_COMPOSE_EVIDENCE_IMAGES"
  done
  for evidence_service in mcp-probe test-runner; do
    evidence_image_ref="${PROJECT_NAME}-${evidence_service}"
    evidence_image_id=$(docker image inspect --format '{{.Id}}' "$evidence_image_ref")
    case "$evidence_image_id" in
      sha256:*) ;;
      *) fail "evidence image ID malformed for $evidence_service" ;;
    esac
    printf '%s\t%s\t%s\n' "$evidence_service" "$evidence_image_ref" "$evidence_image_id" >>"$TASKGATE_COMPOSE_EVIDENCE_IMAGES"
  done
fi

echo "all Compose end-to-end checks passed, including canonical object-store Parquet persistence and delivery"
