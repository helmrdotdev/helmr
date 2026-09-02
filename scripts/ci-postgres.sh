#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
tmp_root=${RUNNER_TEMP:-${TMPDIR:-/tmp}}
workdir=$(mktemp -d "${tmp_root%/}/helmr-postgres.XXXXXX")
pgdata="$workdir/data"
socket_dir="$workdir/socket"
log_file="$workdir/postgres.log"
port=${HELMR_CI_POSTGRES_PORT:-55432}
redis_log="$workdir/redis.log"
redis_port=${HELMR_CI_REDIS_PORT:-56379}
redis_pid=""

cleanup() {
	if [ -n "$redis_pid" ]; then
		kill "$redis_pid" >/dev/null 2>&1 || true
		wait "$redis_pid" >/dev/null 2>&1 || true
	fi
	if [ -d "$pgdata" ]; then
		pg_ctl -D "$pgdata" -m fast -w stop >/dev/null 2>&1 || true
	fi
	if [ "${KEEP_HELMR_CI_POSTGRES:-0}" != "1" ]; then
		rm -rf "$workdir"
	else
		printf 'kept Postgres workdir: %s\n' "$workdir" >&2
	fi
}
trap cleanup EXIT

mkdir -p "$socket_dir"
initdb -D "$pgdata" --username=postgres --auth=trust >/dev/null
cat >>"$pgdata/postgresql.conf" <<EOF
listen_addresses = '127.0.0.1'
port = $port
unix_socket_directories = '$socket_dir'
EOF

if ! pg_ctl -D "$pgdata" -l "$log_file" -w start >/dev/null; then
	cat "$log_file" >&2
	exit 1
fi

export HELMR_TEST_DATABASE_URL="postgres://postgres@127.0.0.1:${port}/postgres?sslmode=disable"
redis-server --bind 127.0.0.1 --port "$redis_port" --save '' --appendonly no >"$redis_log" 2>&1 &
redis_pid=$!
redis_ready=0
for _ in $(seq 1 50); do
	if ! kill -0 "$redis_pid" >/dev/null 2>&1; then
		cat "$redis_log" >&2
		exit 1
	fi
	redis_process_id=""
	if redis_info=$(redis-cli -h 127.0.0.1 -p "$redis_port" info server 2>/dev/null); then
		redis_process_id=$(printf '%s\n' "$redis_info" \
			| awk -F: '$1 == "process_id" { gsub(/\r/, "", $2); print $2 }')
	fi
	if [ "$redis_process_id" = "$redis_pid" ]; then
		redis_ready=1
		break
	fi
	sleep 0.1
done
if [ "$redis_ready" != "1" ]; then
	cat "$redis_log" >&2
	exit 1
fi
export HELMR_TEST_REDIS_URL="redis://127.0.0.1:${redis_port}/0"
cd "$repo_root"
CGO_ENABLED=1 go test -race -count=1 \
	./cmd/internal/dev-controlplane \
	./internal/controlplane \
	./internal/bootstrap \
	./internal/db \
	./internal/db/schema \
	./internal/eventstream \
	./internal/dispatch \
	./internal/idempotency \
	./internal/run \
	./internal/schedule \
	./internal/secret \
	./internal/pglock \
	./internal/token \
	./cmd/control-plane \
	./cmd/dispatcher
