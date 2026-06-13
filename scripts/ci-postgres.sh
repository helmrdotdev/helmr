#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
tmp_root=${RUNNER_TEMP:-${TMPDIR:-/tmp}}
workdir=$(mktemp -d "${tmp_root%/}/helmr-postgres.XXXXXX")
pgdata="$workdir/data"
socket_dir="$workdir/socket"
log_file="$workdir/postgres.log"
port=${HELMR_CI_POSTGRES_PORT:-55432}

cleanup() {
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
cd "$repo_root"
go test ./internal/db ./internal/db/schema ./internal/control ./cmd/helmr-control ./cmd/helmr-dispatcher
