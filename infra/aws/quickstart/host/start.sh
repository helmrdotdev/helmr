#!/bin/sh
set -eu
compose() { docker compose -f /run/helmr/compose.json "$@"; }
compose config --quiet
compose pull
compose run --rm --no-deps controlplane check-config
compose up -d --wait --wait-timeout 180 postgres clickhouse
compose run --rm database-bootstrap
compose run --rm clickhouse-bootstrap
compose run --rm migrate
compose up -d controlplane dispatcher caddy
curl --fail --silent --retry 30 --retry-connrefused --retry-delay 2 http://127.0.0.1:8080/readyz >/dev/null
