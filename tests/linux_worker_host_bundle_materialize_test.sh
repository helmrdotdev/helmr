#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="${root}/scripts/materialize-linux-worker-host-bundle.sh"

rg -F 'nixos/nix:2.31.2@sha256:c7cc6c8cb5d81bed19997247629604708fda95c99c43ac362daa05b6a68e8a24' "${script}" >/dev/null
rg -F 'git -C "${ROOT}" archive --format=tar HEAD | tar -xf - -C "${source_dir}"' "${script}" >/dev/null
rg -F 'path:/work#packages.x86_64-linux.workerHost' "${script}" >/dev/null
rg -F 'source=${source_dir},target=/work,readonly' "${script}" >/dev/null
rg -F '"${ROOT}/scripts/materialize-worker-host-bundle.sh" "${output}" "${host_dir}"' "${script}" >/dev/null
if rg -F 'source=${ROOT},target=/work' "${script}" >/dev/null; then
  printf 'not ok - Worker host builder must not mount Product Git metadata\n' >&2
  exit 1
fi
if rg -F -- '--privileged' "${script}" >/dev/null || rg -F 'seccomp=unconfined' "${script}" >/dev/null; then
  printf 'not ok - Worker host builder must not receive elevated container privileges\n' >&2
  exit 1
fi

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
repo="${tmp}/repo"
host="${tmp}/host"
test_bin="${tmp}/bin"
test_tmp="${tmp}/temp"
mkdir -p "${repo}/scripts" "${host}/bin" "${host}/share/helmr" "${test_bin}" "${test_tmp}"
cp "${script}" "${repo}/scripts/materialize-linux-worker-host-bundle.sh"
cp "${root}/scripts/materialize-worker-host-bundle.sh" "${repo}/scripts/materialize-worker-host-bundle.sh"
chmod 0755 "${repo}/scripts/"*.sh
printf 'tracked\n' >"${repo}/tracked"

for name in cpu-template-helper firecracker helmr-worker jailer mkfs.ext4; do
  printf '#!/usr/bin/env sh\nexit 0\n' >"${host}/bin/${name}"
  chmod 0755 "${host}/bin/${name}"
done
printf '[defaults]\n' >"${host}/share/helmr/mke2fs.conf"
chmod 0444 "${host}/share/helmr/mke2fs.conf"
chmod 0555 "${host}/bin" "${host}/share" "${host}/share/helmr"

cat >"${test_bin}/file" <<'EOF'
#!/usr/bin/env sh
printf '%s: ELF 64-bit LSB executable, x86-64, statically linked\n' "$1"
EOF
cat >"${test_bin}/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$@" >>"${MOCK_DOCKER_ARGS}"
if [ -n "${MOCK_CREATE_OUTPUT:-}" ]; then
  mkdir "${MOCK_CREATE_OUTPUT}"
  printf 'owned by another invocation\n' >"${MOCK_CREATE_OUTPUT}/sentinel"
fi
if [ "${MOCK_DOCKER_FAIL:-0}" = 1 ]; then
  exit 42
fi
tar -C "${MOCK_WORKER_HOST}" -cf - .
EOF
chmod 0755 "${test_bin}/file" "${test_bin}/docker"

git init -q "${repo}"
git -C "${repo}" config user.email test@example.invalid
git -C "${repo}" config user.name test
git -C "${repo}" add .
git -C "${repo}" -c commit.gpgsign=false commit -qm initial
source_commit="$(git -C "${repo}" rev-parse HEAD)"
docker_args="${tmp}/docker.args"

TMPDIR="${test_tmp}" PATH="${test_bin}:${PATH}" MOCK_DOCKER_ARGS="${docker_args}" MOCK_WORKER_HOST="${host}" \
  "${repo}/scripts/materialize-linux-worker-host-bundle.sh" "${tmp}/bundle-a" >/dev/null
TMPDIR="${test_tmp}" PATH="${test_bin}:${PATH}" MOCK_DOCKER_ARGS="${docker_args}" MOCK_WORKER_HOST="${host}" \
  "${repo}/scripts/materialize-linux-worker-host-bundle.sh" "${tmp}/bundle-b" >/dev/null

cmp "${tmp}/bundle-a/worker-host-artifacts.tar" "${tmp}/bundle-b/worker-host-artifacts.tar"
cmp "${tmp}/bundle-a/worker-host-artifacts.json" "${tmp}/bundle-b/worker-host-artifacts.json"
cmp "${tmp}/bundle-a/worker-host-bundle.json" "${tmp}/bundle-b/worker-host-bundle.json"
jq -e --arg source_commit "${source_commit}" '.sourceCommit == $source_commit' \
  "${tmp}/bundle-a/worker-host-bundle.json" >/dev/null
grep -Fxq -- '--platform' "${docker_args}"
grep -Fxq -- 'linux/amd64' "${docker_args}"
grep -Fxq -- 'nixos/nix:2.31.2@sha256:c7cc6c8cb5d81bed19997247629604708fda95c99c43ac362daa05b6a68e8a24' "${docker_args}"
grep -Fq -- 'path:/work#packages.x86_64-linux.workerHost' "${docker_args}"

printf 'dirty\n' >>"${repo}/tracked"
if TMPDIR="${test_tmp}" PATH="${test_bin}:${PATH}" MOCK_DOCKER_ARGS="${tmp}/dirty.args" MOCK_WORKER_HOST="${host}" \
  "${repo}/scripts/materialize-linux-worker-host-bundle.sh" "${tmp}/dirty-output" >/dev/null 2>&1; then
  printf 'not ok - dirty Product source must fail closed\n' >&2
  exit 1
fi
[ ! -e "${tmp}/dirty.args" ]
[ ! -e "${tmp}/dirty-output" ]
git -C "${repo}" checkout -q -- tracked

if TMPDIR="${test_tmp}" PATH="${test_bin}:${PATH}" MOCK_DOCKER_ARGS="${tmp}/failed.args" MOCK_DOCKER_FAIL=1 MOCK_WORKER_HOST="${host}" \
  "${repo}/scripts/materialize-linux-worker-host-bundle.sh" "${tmp}/failed-output" >/dev/null 2>&1; then
  printf 'not ok - failed Linux builder must fail closed\n' >&2
  exit 1
fi
[ ! -e "${tmp}/failed-output" ]

if TMPDIR="${test_tmp}" PATH="${test_bin}:${PATH}" MOCK_DOCKER_ARGS="${tmp}/raced.args" MOCK_DOCKER_FAIL=1 \
  MOCK_CREATE_OUTPUT="${tmp}/raced-output" MOCK_WORKER_HOST="${host}" \
  "${repo}/scripts/materialize-linux-worker-host-bundle.sh" "${tmp}/raced-output" >/dev/null 2>&1; then
  printf 'not ok - losing concurrent materializer must fail\n' >&2
  exit 1
fi
[ "$(cat "${tmp}/raced-output/sentinel")" = 'owned by another invocation' ]
[ -z "$(find "${test_tmp}" -mindepth 1 -print -quit)" ]
find "${host}" -type d -exec chmod u+w {} +

printf 'ok - Linux Worker host bundle materializer contract\n'
