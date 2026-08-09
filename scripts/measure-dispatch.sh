#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
tmp_root=${RUNNER_TEMP:-${TMPDIR:-/tmp}}
workdir=$(mktemp -d "${tmp_root%/}/helmr-dispatch-measure.XXXXXX")
pgdata="$workdir/postgres"
postgres_log="$workdir/postgres.log"
postgres_port=${HELMR_DISPATCH_MEASURE_POSTGRES_PORT:-55437}

cleanup() {
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
# Fixture loading is outside the measured query window. This cluster is
# disposable, so durability writes only add host-dependent setup noise.
pg_ctl -D "$pgdata" -l "$postgres_log" \
	-o "-h 127.0.0.1 -p $postgres_port -c fsync=off -c synchronous_commit=off -c full_page_writes=off" \
	-w start >/dev/null

export HELMR_MEASURE_DISPATCH=1
export HELMR_TEST_DATABASE_URL="postgres://postgres@127.0.0.1:${postgres_port}/postgres?sslmode=disable"

cd "$repo_root"
go test -run '^TestMeasureDispatchHierarchy$' -count=1 -timeout 60m -v ./internal/dispatch
