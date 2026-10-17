# Local Testing Guide

Step-by-step guide to running Flexprice locally and validating changes end-to-end.
Works for humans and AI agents alike — no manual env var configuration needed.

---

## Pre-configured Local API Key

Use this key for all local API calls — it's baked into `.env.local`:

```
x-api-key:        sk_local_flexprice_test_key
x-environment-id: 00000000-0000-0000-0000-000000000000
```

Tenant ID: `00000000-0000-0000-0000-000000000000` · Environment: Sandbox

---

## How local config works

`.env.local` is a committed file that contains all local infrastructure settings.
When you run via `make run-local*` targets, the Makefile sources `.env` first
then `.env.local` on top — so local Docker endpoints override any production
settings in `.env`, without modifying that file.

**Never add** `godotenv.Load(".env.local")` to application code — that would
affect production deployments.

---

## Host-native route (host PostgreSQL/Redis, no Docker PG/Redis)

Use this route when `make dev-setup` conflicts with services already listening
on **5432** (PostgreSQL) or **6379** (Redis). Kafka, ClickHouse, and a Temporal
dev server still run in Docker; PostgreSQL and Redis must already be running on
the host at **loopback** addresses.

**Prerequisites:** `go`, `docker` (Compose v2), `make`, `psql`, `pg_isready`,
and `redis-cli` in `PATH`; a standalone PostgreSQL reachable at
`127.0.0.1:5432` and a standalone Redis answering `PONG` at `127.0.0.1:6379`.

```bash
# First run — bootstrap schema/seeds/topics/tables, then start the app.
# Idempotent: safe to rerun. Mutates the local schema and seeds data.
make setup-local-host

# Daily — validates the existing installation and runs the app in the
# foreground. Does not change schema or seeds.
make run-local-host
```

Equivalent direct invocations: `bash scripts/start-local-host.sh --setup` and
`bash scripts/start-local-host.sh`.

Notes:
- **Loopback only.** The script exports `FLEXPRICE_POSTGRES_HOST=127.0.0.1`,
  `FLEXPRICE_POSTGRES_PORT=5432`, `FLEXPRICE_REDIS_HOST=127.0.0.1`, and
  `FLEXPRICE_REDIS_PORT=6379`, overriding any host values in `.env.local`.
  A remote PostgreSQL/Redis configured there (for example a Tailscale or cloud
  host) is **not** used by this route; it is not a remote-database workflow.
- The script uses the `flexprice` role/database name and reads
  `FLEXPRICE_POSTGRES_PASSWORD` and `FLEXPRICE_CLICKHOUSE_PASSWORD` from
  `.env.local`. If `.env.local` contains external/remote PostgreSQL settings,
  only the password is reused here; the host/port/user/database are forced to
  loopback. That password must match the local host role or bootstrap and
  validation fail — the copied targets cannot use remote PostgreSQL credentials.
- `--setup` creates the `flexprice` login role and database only if missing,
  then runs bootstrap SQL → Ent migrations → remaining SQL migrations
  (including seeds), ClickHouse migrations, and Kafka topic creation. It
  **never resets an existing role's password** and never grants SUPERUSER.
  Schema changes only happen with explicit `--setup`.
- Both the `--setup` and no-arg runs bring up only `kafka`, `clickhouse`, and
  `temporal-dev` (the latter from `docker-compose.host.yml`) and wait until all
  three are healthy. **No PostgreSQL or Redis container is started**, and the
  base stack's Postgres-backed `temporal` service is not started either.
  `docker-compose.host.yml` adds `temporal-dev` with no `depends_on`, so Compose
  does not pull `postgres` or the base `temporal` service.
- Temporal points at the Dockerized **dev server**: both runs export
  `FLEXPRICE_TEMPORAL_ADDRESS=127.0.0.1:7233` (namespace `default`, TLS off)
  and start `temporal-dev`. The script does **not** set
  `FLEXPRICE_TEMPORAL_ENABLED`; it only forces the address, namespace, and TLS
  settings. The dev server persists state in a SQLite file
  on the `temporal-dev-data` Docker volume and exposes its UI at
  http://localhost:8233 — local development only, never use it in production.
- **The repo has no `temporal.enabled` config key**: the
  `FLEXPRICE_TEMPORAL_ENABLED=false` line in `.env.local` does **not** disable
  the Temporal client. Local mode always dials `FLEXPRICE_TEMPORAL_ADDRESS` at
  startup, so use this route (which starts `temporal-dev`) instead of toggling
  that flag by hand.
- The default (cache-disabled) profile is preserved; Redis is still connected
  for locks.
- `Ctrl+C` stops only the app — the Kafka/ClickHouse/Temporal dev containers
  and host PostgreSQL/Redis keep running.
- First run downloads Go modules before the server compiles. If the default
  download stalls, retry with a per-command proxy override (no global
  `go env`/sumdb changes):

  ```bash
  GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct make setup-local-host
  ```

Smoke check once the server is up:

```bash
curl http://localhost:8080/health
# → {"status":"ok"}

curl http://localhost:8080/v1/meters \
  -H "x-api-key: sk_local_flexprice_test_key" \
  -H "x-environment-id: 00000000-0000-0000-0000-000000000000"
# → {"items":[...],"pagination":{...}}
```

---

## Quick Start

### 1. Start OrbStack

```bash
open -a OrbStack && sleep 3
```

### 2. Start local infrastructure

```bash
docker compose up -d postgres kafka clickhouse
sleep 10   # wait for Kafka to fully initialise
```

### 3. Create Kafka topics

Run once (idempotent — safe to repeat):

```bash
for topic in raw_events events events_lazy events_post_processing \
             events_backfill system_events onboarding_events balance_alert; do
  docker compose exec kafka kafka-topics --bootstrap-server kafka:9092 \
    --create --topic $topic --partitions 3 --replication-factor 1 2>/dev/null || true
done
```

### 4. Run migrations

```bash
# Postgres SQL migrations (reads docker-compose defaults, safe to run as-is)
make migrate-postgres

# Ent schema migrations against local Postgres (uses .env.local)
make migrate-local

# ClickHouse migrations
make migrate-clickhouse
```

> **Warning:** `make migrate-ent` reads `.env` and will run against production
> if that file points there. Always use `make migrate-local` instead.

### 5. Run the server

**Two separate terminals (mirrors production deployment):**

```bash
# Terminal 1 — API server
make run-local-api

# Terminal 2 — Kafka consumer
make run-local-consumer
```

**Or everything in one process (quickest for local dev):**

```bash
make run-local
```

### 6. Verify

```bash
curl http://localhost:8080/health
# → {"status":"ok"}

curl http://localhost:8080/v1/meters \
  -H "x-api-key: sk_local_flexprice_test_key" \
  -H "x-environment-id: 00000000-0000-0000-0000-000000000000"
# → {"items":[],"pagination":{...}}
```

---

## Shutdown

```bash
# Stop Go processes (Ctrl+C in each terminal, or:)
pkill -f "go run cmd/server"

# Stop Docker services
docker compose down
```

---

## Useful DB Commands

> On the host-native route there is **no `postgres` container**; connect to the
> host server directly, e.g. `psql -h 127.0.0.1 -p 5432 -U flexprice -d flexprice`.

```bash
# PostgreSQL shell (Docker-backed route only)
docker compose exec postgres psql -U flexprice -d flexprice

# ClickHouse shell
docker compose exec clickhouse clickhouse-client \
  --user=flexprice --password=flexprice123 --database=flexprice

# Quick ClickHouse queries
docker compose exec clickhouse clickhouse-client \
  --user=flexprice --password=flexprice123 --database=flexprice \
  --query="SELECT id, external_customer_id, event_name FROM events ORDER BY timestamp DESC LIMIT 10 FORMAT PrettyCompact"
```

---

## Local Infrastructure Reference

| Service | Host:Port | User | Password | DB |
|---------|-----------|------|----------|----|
| PostgreSQL | `localhost:5432` | `flexprice` | `flexprice123` | `flexprice` |
| ClickHouse | `localhost:9000` | `flexprice` | `flexprice123` | `flexprice` |
| Kafka | `localhost:29092` | — | — | — |

| Seed record | ID |
|-------------|----|
| Default Tenant | `00000000-0000-0000-0000-000000000000` |
| Sandbox Environment | `00000000-0000-0000-0000-000000000000` |
| Production Environment | `00000000-0000-0000-0000-000000000001` |

---

## Common Pitfalls

| Problem | Cause | Fix |
|---------|-------|-----|
| `make migrate-ent` or `make migrate-local` hits production | `.env` loaded, not `.env.local` | Always use `make migrate-local` |
| `address already in use :8080` | Another process on 8080 | `lsof -ti:8080 \| xargs kill` |
| Consumer: `topic does not exist` errors | Topic not created | Run the topic creation loop in Step 3 |
| `{"error":"Unauthorized"}` | Wrong API key | Use `sk_local_flexprice_test_key` from `.env.local` |
| ClickHouse `raw_events` table is empty | Consumer writes to `events`, not `raw_events` | Check the `events` table — that's where transformed events land |
| Settings lookup fails | Missing `x-environment-id` header | Always send `-H "x-environment-id: 00000000-0000-0000-0000-000000000000"` |

---

## See also

- [`SANITY_CHECK.md`](SANITY_CHECK.md) — standard pre-validation checklist to run before every PR
