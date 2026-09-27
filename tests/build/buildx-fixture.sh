#!/usr/bin/env bash
# Native local-registry fixture isolation; never select or remove a user builder.
start_buildx_fixture() {
  local work=$1 selected endpoint proposed_context
  selected=${DOCKER_CONTEXT:-$(docker context show)}
  endpoint=$(docker context inspect "$selected" --format '{{.Endpoints.docker.Host}}')
  case "$endpoint" in unix://*) ;; *) echo 'local Docker fixture requires a Unix-socket daemon' >&2; return 1 ;; esac
  fixture_original_context=$(env -u DOCKER_CONTEXT -u DOCKER_HOST docker context show)
  proposed_context="helmr-fixture-$$"
  docker context create "$proposed_context" --docker "host=$endpoint" >/dev/null || return $?
  fixture_context=$proposed_context
  export DOCKER_CONTEXT=$fixture_context BUILDX_CONFIG="$work/buildx"
  unset DOCKER_HOST BUILDX_BUILDER
  mkdir -p "$BUILDX_CONFIG"
  fixture_builder=$(python3 - "$fixture_context" "$endpoint" <<'PY'
import hashlib,sys
print('helmr-'+hashlib.sha256((sys.argv[1]+'\n'+sys.argv[2]).encode()).hexdigest()[:24])
PY
)
}
cleanup_buildx_fixture() {
  if [ -n "${fixture_builder:-}" ]; then docker buildx rm "$fixture_builder" >/dev/null 2>&1 || true; fi
  if [ -n "${fixture_context:-}" ]; then docker --context default context rm "$fixture_context" >/dev/null 2>&1 || true; fi
  if [ -n "${fixture_original_context:-}" ]; then
    [ "$(env -u DOCKER_CONTEXT -u DOCKER_HOST docker context show)" = "$fixture_original_context" ] || { echo "fixture changed global Docker context" >&2; return 1; }
  fi
}
