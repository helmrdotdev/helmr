#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
if [ "${1:-}" = --help ]; then
  echo 'usage: scripts/ci-postgres.sh [[-v] TOP_LEVEL_TEST_PATTERN PACKAGE...]'
  echo 'No arguments runs the CI suite. A selection must execute passing, non-skipped tests in every package.'
  echo '-v streams the selected tests'"'"' output while they run.'
  exit 0
fi
if [ "$#" = 1 ]; then
  echo 'a selected test pattern requires at least one Go package' >&2
  exit 2
fi
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
if [ "$#" -gt 0 ]; then
  CGO_ENABLED=1 GOFLAGS="${GOFLAGS:-} -race" bash "$repo_root/scripts/test-go-selection.sh" "$@"
  exit 0
fi
CGO_ENABLED=1 go test -race -count=1 \
	./dev/runtime \
	./cmd/internal/dev-controlplane \
	./internal/session \
	./internal/command \
	./internal/computer \
	./internal/controlplane \
	./internal/db \
	./internal/db/dbpool \
	./internal/db/schema \
	./internal/deployment \
	./internal/idempotency \
	./internal/identity \
	./internal/org \
	./internal/secret \
	./internal/slack \
	./internal/sqlpolicy \
	./internal/telemetry \
	./internal/pglock \
	./internal/region \
	./internal/workergroup \
	./cmd/control-plane \
	./cmd/dispatcher
