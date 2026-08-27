#!/usr/bin/env bash
# Bring up the isolated, digest-pinned PostgreSQL environment the
# database-backed tests need, and print the DSNs that enable them.
#
#   scripts/db-test-env.sh up       start, install sidecars, wait for health
#   scripts/db-test-env.sh env      print the exports for a test run
#   scripts/db-test-env.sh verify   prove the environment is the frozen one
#   scripts/db-test-env.sh test ... run `go test` with the DSNs exported
#   scripts/db-test-env.sh down     stop and REMOVE the volumes
#
# The credentials below are test-only and deliberately committed: they make a
# test run reproducible and they reach nothing but loopback-bound containers
# that `down` destroys. None of them is used by a deployment, and none is read
# from a developer's .env -- a harness that silently inherited one would run
# against whatever that developer happened to have configured.
set -euo pipefail

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

export DBTEST_CONTROL_PORT=${DBTEST_CONTROL_PORT:-25533}
export DBTEST_BUSINESS_PORT=${DBTEST_BUSINESS_PORT:-25534}

export CONTROL_POSTGRES_DB=taskbound_gateway
export CONTROL_POSTGRES_ADMIN_PASSWORD=dbtest-control-admin
export CONTROL_DB_PASSWORD=dbtest-control-app
export POSTGRES_DB=travel_demo
export POSTGRES_USER=postgres
export POSTGRES_PASSWORD=dbtest-business-admin
export GATEWAY_DB_PASSWORD=dbtest-gateway-reader

# compose.yaml declares these required for services this harness never starts.
# Compose still parses the whole file, so they must be present; they are inert.
export GATEWAY_DATA_KEY=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
export GATEWAY_RECEIPT_KEY_ID=dbtest-gateway-ed25519-v1
export GATEWAY_RECEIPT_PRIVATE_KEY=AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=
export GATEWAY_OBJECT_STORE_ACCESS_KEY=dbtest-gateway
export GATEWAY_OBJECT_STORE_SECRET_KEY=dbtest-object-store-secret
export MINIO_ROOT_USER=dbtest-minio-admin
export MINIO_ROOT_PASSWORD=dbtest-minio-root-password
export TASKBOUND_ALICE_TOKEN=dbtest-alice-token
export TASKBOUND_CAROL_TOKEN=dbtest-carol-token
export OA_SERVICE_TOKEN=dbtest-oa-service-token
export OA_CALLBACK_SECRET=dbtest-oa-callback-secret
export OA_RECEIPT_KEY_ID=dbtest-oa-ed25519-v1
export OA_RECEIPT_PRIVATE_KEY=nWGxne/9WmC6hEr0kuwsxERJxWl7MmkZcDusAxyuf2A=
export OA_SESSION_SECRET=dbtest-oa-session-secret
export OA_ALICE_PASSWORD=dbtest-alice-password
export OA_BOB_PASSWORD=dbtest-bob-password

COMPOSE=(docker compose
  -f compose.yaml
  -f evaluation/final-v5-wsl2/compose.real-pilot.yaml
  -f evaluation/final-v5-wsl2/compose.observer-v3.yaml
  -f compose.dbtest.yaml)

control_dsn() {
  printf 'postgres://postgres:%s@127.0.0.1:%s/%s?sslmode=disable' \
    "$CONTROL_POSTGRES_ADMIN_PASSWORD" "$DBTEST_CONTROL_PORT" "$CONTROL_POSTGRES_DB"
}

# The Business DSN connects as gateway_reader, not as the superuser. The
# publication and RLS checks are about what that role can see, and running them
# as postgres would bypass the very controls under test.
business_dsn() {
  printf 'postgres://gateway_reader:%s@127.0.0.1:%s/%s?sslmode=disable' \
    "$GATEWAY_DB_PASSWORD" "$DBTEST_BUSINESS_PORT" "$POSTGRES_DB"
}

business_admin_dsn() {
  printf 'postgres://postgres:%s@127.0.0.1:%s/%s?sslmode=disable' \
    "$POSTGRES_PASSWORD" "$DBTEST_BUSINESS_PORT" "$POSTGRES_DB"
}

wait_healthy() {
  local service=$1 deadline=$((SECONDS + 900))
  while (( SECONDS < deadline )); do
    local id status
    id=$("${COMPOSE[@]}" ps -q "$service" 2>/dev/null || true)
    if [[ -n "$id" ]]; then
      status=$(docker inspect --format '{{.State.Health.Status}}' "$id" 2>/dev/null || echo starting)
      [[ "$status" == healthy ]] && return 0
      if [[ "$status" == unhealthy ]]; then
        echo "$service became unhealthy" >&2
        "${COMPOSE[@]}" logs --tail 60 "$service" >&2
        return 1
      fi
    fi
    sleep 2
  done
  echo "$service did not become healthy within 900s" >&2
  "${COMPOSE[@]}" logs --tail 60 "$service" >&2
  return 1
}

# Wait for a one-shot job service to exit, and require exit status 0.
wait_completed() {
  local service=$1 deadline=$((SECONDS + 900))
  while (( SECONDS < deadline )); do
    local id running code
    id=$("${COMPOSE[@]}" ps -aq "$service" 2>/dev/null || true)
    if [[ -n "$id" ]]; then
      running=$(docker inspect --format '{{.State.Running}}' "$id" 2>/dev/null || echo true)
      if [[ "$running" == false ]]; then
        code=$(docker inspect --format '{{.State.ExitCode}}' "$id" 2>/dev/null || echo unknown)
        [[ "$code" == 0 ]] && return 0
        echo "$service exited with status $code" >&2
        "${COMPOSE[@]}" logs --tail 60 "$service" >&2
        return 1
      fi
    fi
    sleep 2
  done
  echo "$service did not complete within 900s" >&2
  "${COMPOSE[@]}" logs --tail 60 "$service" >&2
  return 1
}

case "${1:-}" in
  up)
    # Down with volumes first. A reused data directory means initdb never
    # reruns, so a db/init change would be silently absent and this run's
    # result would depend on a previous one's.
    "${COMPOSE[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
    # snapshot-sidecar-install pulls in business-postgres and the three
    # snapshot-index builders through its own depends_on, so the ordinal
    # sidecars and snapshot artifacts the gateway tests read are published
    # before any test runs. Requesting only the two servers was the earlier
    # mistake: the tests then failed on a missing taskgate_ordinal relation.
    "${COMPOSE[@]}" up -d --wait control-postgres snapshot-sidecar-install
    wait_healthy control-postgres
    wait_healthy business-postgres
    # `--wait` returns once the one-shot installer is *running*, not once it has
    # published: measured 2026-08-27, `verify` straight after `up` saw
    # taskgate_ordinal relations=0 and the same environment showed 35 a minute
    # later with the installer Exited (0). Wait for the job to exit and require
    # exit 0, as the campaign runner does for its phase-1 jobs.
    wait_completed snapshot-sidecar-install
    echo "control  $(control_dsn)"
    echo "business $(business_dsn)"
    ;;

  env)
    echo "export CONTROL_TEST_POSTGRES_DSN='$(control_dsn)'"
    echo "export BUSINESS_TEST_POSTGRES_DSN='$(business_dsn)'"
    echo "export BUSINESS_ADMIN_TEST_POSTGRES_DSN='$(business_admin_dsn)'"
    echo "export TASKGATE_FINAL_V5_BUSINESS_DSN='$(business_dsn)'"
    echo "export EXPOSURE_TEST_POSTGRES_DSN='$(control_dsn)'"
    echo "export TASKGATE_FINAL_V5_SQLCHECK_ADMIN_DSN='$(business_admin_dsn)'"
    ;;

  verify)
    fail=0
    # server_version_num is the exact number RequiredMeasurementEnvironment
    # demands. A different build is a different measurement environment, not a
    # close-enough one.
    for pair in "control-postgres:$CONTROL_POSTGRES_DB" "business-postgres:$POSTGRES_DB"; do
      service=${pair%%:*}; database=${pair##*:}
      version=$("${COMPOSE[@]}" exec -T "$service" \
        psql -X -qtA -U postgres -d "$database" -c 'SHOW server_version_num' | tr -d '\r')
      printf '%-18s server_version_num=%s' "$service" "$version"
      if [[ "$version" == 160014 ]]; then echo "  OK"; else echo "  WANT 160014"; fail=1; fi
    done

    echo "--- business extensions, roles, schemas ---"
    "${COMPOSE[@]}" exec -T business-postgres \
      psql -X -qtA -U postgres -d "$POSTGRES_DB" -c \
      "SELECT 'extension '||extname||' '||extversion FROM pg_extension ORDER BY extname" | tr -d '\r'
    "${COMPOSE[@]}" exec -T business-postgres \
      psql -X -qtA -U postgres -d "$POSTGRES_DB" -c \
      "SELECT 'role '||rolname FROM pg_roles WHERE rolname = 'gateway_reader'" | tr -d '\r'
    "${COMPOSE[@]}" exec -T business-postgres \
      psql -X -qtA -U postgres -d "$POSTGRES_DB" -c \
      "SELECT 'schema '||nspname FROM pg_namespace WHERE nspname NOT LIKE 'pg\\_%' AND nspname <> 'information_schema' ORDER BY nspname" | tr -d '\r'

    # The ordinal sidecar is what a bare two-server environment lacks, and its
    # absence is the failure that motivated composing on the real topology.
    echo "--- ordinal sidecars ---"
    sidecars=$("${COMPOSE[@]}" exec -T business-postgres \
      psql -X -qtA -U postgres -d "$POSTGRES_DB" -c \
      "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
       WHERE n.nspname = 'taskgate_ordinal'" | tr -d '\r')
    printf 'taskgate_ordinal relations=%s' "$sidecars"
    if [[ "${sidecars:-0}" -gt 0 ]]; then echo "  OK"; else echo "  MISSING"; fail=1; fi

    # pg_stat_statements settings decide what the observer can measure at all.
    echo "--- pg_stat_statements ---"
    if ! "${COMPOSE[@]}" exec -T business-postgres \
      psql -X -qtA -U postgres -d "$POSTGRES_DB" -c \
      "SELECT 'track='||current_setting('pg_stat_statements.track')||
              ' track_utility='||current_setting('pg_stat_statements.track_utility')||
              ' track_planning='||current_setting('pg_stat_statements.track_planning')" 2>/dev/null | tr -d '\r'; then
      echo "pg_stat_statements is not loaded" >&2; fail=1
    fi
    "${COMPOSE[@]}" exec -T business-postgres \
      psql -X -qtA -U postgres -d "$POSTGRES_DB" -c \
      "SELECT 'stats_reset='||stats_reset||' dealloc='||dealloc FROM pg_stat_statements_info" 2>/dev/null | tr -d '\r' || true

    echo "--- control role ---"
    "${COMPOSE[@]}" exec -T control-postgres \
      psql -X -qtA -U postgres -d "$CONTROL_POSTGRES_DB" -c \
      "SELECT 'role '||rolname FROM pg_roles WHERE rolname = 'gateway_control'" | tr -d '\r'

    exit "$fail"
    ;;

  test)
    shift
    export CONTROL_TEST_POSTGRES_DSN="$(control_dsn)"
    export BUSINESS_TEST_POSTGRES_DSN="$(business_dsn)"
    # BUSINESS_ADMIN_TEST_POSTGRES_DSN reaches the deployment that HAS
    # pg_stat_statements, as a role that may reset it. Before it existed the
    # three tests about what that server retains -- the pin domain-separation
    # proof and both halves of the strict-AST C3 gate -- read the control-store
    # DSN and skipped with "pg_stat_statements is not installed on this
    # deployment" on a harness where it demonstrably is.
    export BUSINESS_ADMIN_TEST_POSTGRES_DSN="$(business_admin_dsn)"
    # The live compiler fixture reads this. It was skipping as "live compiler
    # PostgreSQL DSN is not configured" against a harness whose business server
    # already carries the final_v5_compiler schema it needs.
    export TASKGATE_FINAL_V5_BUSINESS_DSN="$(business_dsn)"
    # TASKGATE_FINAL_V5_SQLCHECK_ADMIN_DSN is deliberately NOT exported here,
    # even though `env` prints it. The two probe-equivalence tests it enables
    # provision their own benchmark dataset and require a database that does NOT
    # already carry the frozen final_v5_benchmark schema; this harness's business
    # server is provisioned with it by db/init, so they fail on
    # `schema "final_v5_benchmark" already exists` rather than skipping. They
    # need a bare PostgreSQL 16.14 admin DSN of their own. Exporting it here
    # would convert a visible, explained skip into a failure that says nothing
    # about the probe rename it is supposed to check.
    export EXPOSURE_TEST_POSTGRES_DSN="$(control_dsn)"
    export GOFLAGS=${GOFLAGS:--buildvcs=false}
    # go test -timeout is a per-test-binary budget, not a wall-clock budget for
    # the whole invocation: `go help testflag` says, "If a test binary runs
    # longer than duration d, panic." internal/gateway alone took 4016.427s in
    # P6-scale-C2 against a real control store, and its runtime grows with the
    # number of publications in the master Catalog. The 120-minute default
    # leaves about 1.8x headroom; if this package times out again, suspect the
    # budget before treating it as a hang. Each V9 live test installs a Catalog
    # V4 snapshot registry, and compiling those ordinal artifacts is genuinely
    # expensive. Without an explicit timeout the DSN-enabled suite panics
    # mid-package and reports a failure that looks like a hang but is only a
    # budget.
    #
    # This was invisible while the DB-backed tests skipped for want of these
    # DSNs: the first run that actually exercised them was the first run that
    # could hit it.
    timeout_supplied=false
    for argument in "$@"; do
      case "$argument" in
        -timeout|-timeout=*|--timeout|--timeout=*) timeout_supplied=true ;;
      esac
    done
    if [ "$timeout_supplied" = true ]; then
      exec go test "$@"
    fi
    exec go test -timeout="${TASKGATE_DB_TEST_TIMEOUT:-120m}" "$@"
    ;;

  down)
    "${COMPOSE[@]}" down --volumes --remove-orphans
    ;;

  *)
    sed -n '2,15p' "$0" >&2
    exit 2
    ;;
esac
