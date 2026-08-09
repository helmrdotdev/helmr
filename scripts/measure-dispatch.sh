#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
tmp_root=${RUNNER_TEMP:-${TMPDIR:-/tmp}}
workdir=$(mktemp -d "${tmp_root%/}/helmr-dispatch-measure.XXXXXX")
pgdata="$workdir/postgres"
postgres_log="$workdir/postgres.log"
valkey_log="$workdir/valkey.log"
postgres_port=${HELMR_DISPATCH_MEASURE_POSTGRES_PORT:-55437}
valkey_socket="$workdir/valkey.sock"
valkey_pid=

cleanup() {
	if [ -n "$valkey_pid" ]; then
		kill "$valkey_pid" >/dev/null 2>&1 || true
		wait "$valkey_pid" >/dev/null 2>&1 || true
	fi
	if [ -d "$pgdata" ]; then
		pg_ctl -D "$pgdata" -m fast -w stop >/dev/null 2>&1 || true
	fi
	if [ "${KEEP_HELMR_DISPATCH_MEASURE:-0}" = "1" ]; then
		printf 'kept dispatch measurement workdir: %s\n' "$workdir" >&2
		return
	fi
	case "$workdir" in
	"${tmp_root%/}"/helmr-dispatch-measure.*) rm -rf -- "$workdir" ;;
	*) printf 'refusing to remove unexpected measurement path: %s\n' "$workdir" >&2 ;;
	esac
}
trap cleanup EXIT

initdb -D "$pgdata" --username=postgres --auth=trust >/dev/null
pg_ctl -D "$pgdata" -l "$postgres_log" -o "-h 127.0.0.1 -p $postgres_port" -w start >/dev/null

valkey-server \
	--port 0 \
	--unixsocket "$valkey_socket" \
	--unixsocketperm 700 \
	--save '' \
	--appendonly no \
	--dir "$workdir" \
	>"$valkey_log" 2>&1 &
valkey_pid=$!
for _ in $(seq 1 50); do
	if valkey-cli -s "$valkey_socket" ping >/dev/null 2>&1; then
		break
	fi
	sleep 0.1
done
if ! valkey-cli -s "$valkey_socket" ping >/dev/null 2>&1; then
	cat "$valkey_log" >&2
	exit 1
fi

export HELMR_MEASURE_DISPATCH=1
export HELMR_MEASURE_DISPATCH_EXPLAIN=1
export HELMR_TEST_DATABASE_URL="postgres://postgres@127.0.0.1:${postgres_port}/postgres?sslmode=disable"
export HELMR_TEST_REDIS_NETWORK=unix
export HELMR_TEST_REDIS_ADDR="$valkey_socket"

cd "$repo_root"
go test -run '^TestMeasureDispatchDiscovery$' -count=1 -v ./internal/dispatch
go test -run '^TestMeasureReadyQueue$' -count=1 -v ./internal/dispatch/redis
