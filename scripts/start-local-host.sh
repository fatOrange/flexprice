#!/usr/bin/env bash
# start-local-host.sh — FlexPrice on host PostgreSQL/Redis + Compose Kafka/ClickHouse/Temporal.
# The Compose dependency layer is docker-compose.dev.yml; the root
# docker-compose.yml is the released SWR deployment (external PostgreSQL).
set -Eeuo pipefail
IFS=$'\n\t'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

ENV_FILE="$REPO_ROOT/.env.local"
PG_HOST=127.0.0.1
PG_PORT=5432
PG_DB=flexprice
PG_USER=flexprice
PG_BOOTSTRAP_DB=postgres
CH_MEMORY='--max_memory_usage=90000000000'
# Keep in sync with `make init-kafka` / configs/deploy/kafka-topics.json.
TOPICS=(events events_lazy events_bulk events_dlq events_backfill
        event_processing_dlq event_processing_lazy_dlq
        meter_usage_tracking_service_dlq meter_usage_tracking_service_lazy_dlq
        system_events flexprice_system_events wallet_alert onboarding_events raw_events prod_events_v4
        analytics.meter_usage.sink.realtime activity_logs activity_logs_dlq)

usage() {
  cat <<'EOF'
Usage: bash scripts/start-local-host.sh [--setup|--help]

  (no args)  Validate host PostgreSQL/Redis + Compose Kafka/ClickHouse/Temporal
             dev server, then exec 'go run cmd/server/main.go' in the foreground.
  --setup    Idempotent bootstrap (never overwrites existing roles/data):
             PG role/db if missing, V0 -> Ent -> V1..V5, ClickHouse migrations
             (strict), Kafka topics; then validate and start the app.
  --help     Show this help and exit.

Prerequisites: go, docker compose v2+, make, psql/pg_isready (PG18 client),
standalone Redis on 127.0.0.1:6379. Kafka, ClickHouse and the Temporal dev
server run via Docker (temporal-dev: SQLite persistence in a named volume,
gRPC 127.0.0.1:7233, Web UI http://127.0.0.1:8233).
EOF
}

die() { echo "ERROR: $*" >&2; exit 1; }

# Args are parsed before touching env files: --help and bad args stay side-effect-free.
DO_SETUP=false
for arg in "$@"; do
  case "$arg" in
    --help) usage; exit 0 ;;
    --setup) DO_SETUP=true ;;
    *) usage >&2; die "unknown argument: $arg" ;;
  esac
done

[ -f "$ENV_FILE" ] || die ".env.local not found at repo root"
set -a
. "$ENV_FILE"
set +a
# libpq's hostaddr/service settings can override an explicit loopback host.
unset PGHOST PGHOSTADDR PGPORT PGUSER PGPASSWORD PGDATABASE PGSERVICE \
  PGSERVICEFILE PGPASSFILE PGOPTIONS PGSSLMODE PGCONNECT_TIMEOUT \
  PGTARGETSESSIONATTRS PGLOADBALANCEHOSTS
export PGSSLMODE=disable PGCONNECT_TIMEOUT=5


# Force the loopback topology; exported vars win over any file/config defaults.
export FLEXPRICE_DEPLOYMENT_MODE=local
export FLEXPRICE_TEMPORAL_ADDRESS=127.0.0.1:7233
export FLEXPRICE_TEMPORAL_NAMESPACE=default
export FLEXPRICE_TEMPORAL_TLS=false
export FLEXPRICE_POSTGRES_HOST=$PG_HOST
export FLEXPRICE_POSTGRES_PORT=$PG_PORT
export FLEXPRICE_POSTGRES_USER=$PG_USER
export FLEXPRICE_POSTGRES_DBNAME=$PG_DB
export FLEXPRICE_POSTGRES_READER_HOST=$PG_HOST
export FLEXPRICE_REDIS_HOST=127.0.0.1
export FLEXPRICE_REDIS_PORT=6379
export FLEXPRICE_REDIS_CLUSTER_MODE=false
export FLEXPRICE_KAFKA_BROKERS=localhost:29092
export FLEXPRICE_CLICKHOUSE_ADDRESS=localhost:9000
# Align the webhook consumer with the topic set created by `make init-kafka`
# (config.yaml's default is flexprice_system_events).
export FLEXPRICE_WEBHOOK_TOPIC=system_events

: "${FLEXPRICE_POSTGRES_PASSWORD:?FLEXPRICE_POSTGRES_PASSWORD missing in .env.local}"
: "${FLEXPRICE_CLICKHOUSE_PASSWORD:?FLEXPRICE_CLICKHOUSE_PASSWORD missing in .env.local}"

for tool in go docker psql pg_isready redis-cli make; do
  command -v "$tool" >/dev/null 2>&1 || die "required tool '$tool' not found in PATH"
done
docker compose version >/dev/null 2>&1 || die "docker compose plugin not available"
DCV="$(docker compose version --short 2>/dev/null || true)"
DCV="${DCV#v}"
case "${DCV%%.*}" in ''|*[!0-9]*) die "cannot determine docker compose version (got: ${DCV:-empty})" ;; esac
[ "${DCV%%.*}" -ge 2 ] || die "docker compose v2+ required (got ${DCV%%.*}.x)"

pg_ready() { pg_isready -h "$PG_HOST" -p "$PG_PORT" >/dev/null 2>&1; }

# AUTH errors may still exit 0: require an exact PONG.
redis_ok() { [ "$(redis-cli -h 127.0.0.1 -p 6379 ping 2>/dev/null)" = "PONG" ]; }

# Host run composes the base stack with the temporal-dev overlay; postgres,
# redis and the base temporal services stay unused on the host path.
compose() { docker compose -f docker-compose.dev.yml -f docker-compose.host.yml "$@"; }
compose_wait() { compose up -d --wait "$@"; }
compose_exec() { compose exec -T "$@"; }

ch_exec() {
  compose_exec clickhouse clickhouse-client \
    --user flexprice --password "$FLEXPRICE_CLICKHOUSE_PASSWORD" \
    --database flexprice "$CH_MEMORY" "$@"
}

# The EXTERNAL listener binds the container IP, not localhost inside it.
kafka_exec() { compose_exec kafka kafka-topics --bootstrap-server kafka:9092 "$@"; }

# Admin session uses the local socket as the OS user; -w never prompts.
pg_admin() {
  psql -X -w -v ON_ERROR_STOP=1 -p "$PG_PORT" -d "$PG_BOOTSTRAP_DB" "$@"
}

app_psql() {
  PGPASSWORD="$FLEXPRICE_POSTGRES_PASSWORD" psql -X -w -v ON_ERROR_STOP=1 \
    -h "$PG_HOST" -p "$PG_PORT" -U "$PG_USER" -d "$PG_DB" "$@"
}

setup_postgres() {
  echo "==> PostgreSQL: checking role '$PG_USER' / db '$PG_DB'"
  # \gexec runs the generated CREATE only when the role/db is absent.
  pg_admin -v pg_user="$PG_USER" -v pg_db="$PG_DB" \
    -v pg_password="$FLEXPRICE_POSTGRES_PASSWORD" <<'SQL' >/dev/null
SELECT 'CREATE ROLE ' || format('%I', :'pg_user')
       || ' LOGIN PASSWORD ' || format('%L', :'pg_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'pg_user') \gexec
SELECT 'CREATE DATABASE ' || format('%I', :'pg_db')
       || ' OWNER ' || format('%I', :'pg_user')
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = :'pg_db') \gexec
SQL
  app_psql -Atc "SELECT current_user" >/dev/null || \
    die "cannot authenticate as $PG_USER over TCP — check FLEXPRICE_POSTGRES_PASSWORD in .env.local"
}

setup_schema() {
  echo "==> PostgreSQL: V0 bootstrap -> Ent migration -> V1..V5"
  app_psql -f migrations/postgres/V0__init.sql
  go run ./cmd/migrate postgres --timeout 300
  local f
  for f in migrations/postgres/V1__seed.sql \
           migrations/postgres/V2_invoice_sequences.up.sql \
           migrations/postgres/V3__rename_trial_period_to_trial_period_days.up.sql \
           migrations/postgres/V4_prices_sequence.up.sql \
           migrations/postgres/V5__settings_unique_published_only.up.sql; do
    app_psql -f "$f"
  done
}

setup_clickhouse() {
  echo "==> ClickHouse: applying migrations/clickhouse/*.sql (strict)"
  local f
  for f in migrations/clickhouse/*.sql; do
    [ -f "$f" ] || continue
    compose_exec clickhouse clickhouse-client \
      --user flexprice --password "$FLEXPRICE_CLICKHOUSE_PASSWORD" \
      --database flexprice "$CH_MEMORY" --multiquery < "$f" || \
      die "ClickHouse migration failed: $f — fix and re-run --setup (idempotent)"
  done
}

setup_kafka() {
  echo "==> Kafka: make init-kafka"
  make init-kafka
}

validate_postgres() {
  local out
  out="$(app_psql -Atc "SELECT to_regclass('public.tenants') IS NOT NULL AND to_regclass('public.environments') IS NOT NULL")" || \
    die "cannot query $PG_DB at $PG_HOST:$PG_PORT — run --setup to bootstrap"
  [ "$out" = "t" ] || die "schema incomplete (tenants/environments missing) — run --setup"
  out="$(app_psql -Atc "SELECT COUNT(*) FROM public.tenants WHERE id = '00000000-0000-0000-0000-000000000000'")" || \
    die "cannot query tenants — run --setup"
  [ "$out" = "1" ] || die "default tenant not seeded — run --setup"
  out="$(app_psql -Atc "SELECT COUNT(*) FROM public.environments WHERE id = '00000000-0000-0000-0000-000000000000'")" || \
    die "cannot query environments — run --setup"
  [ "$out" = "1" ] || die "default environment not seeded — run --setup"
  echo "==> PostgreSQL: schema + seed OK"
}

validate_kafka() {
  local existing missing="" t
  existing="$(kafka_exec --list 2>/dev/null)" || \
    die "cannot list Kafka topics — check 'docker compose -f docker-compose.dev.yml ps kafka'"
  for t in "${TOPICS[@]}"; do
    grep -qx "$t" <<<"$existing" || missing="$missing $t"
  done
  [ -z "$missing" ] || die "missing Kafka topics:$missing — run --setup"
  echo "==> Kafka: ${#TOPICS[@]} topics OK"
}

validate_clickhouse() {
  local out
  out="$(ch_exec --query "SELECT count() FROM system.tables WHERE database = 'flexprice' AND name = 'events'")" || \
    die "ClickHouse not reachable — check 'docker compose -f docker-compose.dev.yml ps clickhouse'"
  [ "$out" = "1" ] || die "ClickHouse: flexprice.events table missing — run --setup"
  echo "==> ClickHouse: ready (events table present)"
}

pg_ready || die "PostgreSQL not reachable at $PG_HOST:$PG_PORT — start Homebrew PostgreSQL 18 first"
redis_ok || die "Redis at 127.0.0.1:6379 did not answer PONG — start standalone Redis first"

compose_wait kafka clickhouse temporal-dev

if $DO_SETUP; then
  setup_postgres
  setup_schema
  setup_clickhouse
  setup_kafka
  echo "==> Setup complete"
fi

validate_postgres
validate_kafka
validate_clickhouse

echo "==> Starting FlexPrice (local mode: host PostgreSQL/Redis, Compose Kafka/ClickHouse/Temporal)"
exec go run cmd/server/main.go
