#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="${root}/scripts/publish-materialized-platform-release.sh"

rg -q 'nixos/nix:[^@[:space:]]+@sha256:[0-9a-f]{64}' "${script}"
rg -F 'git -C "${ROOT}" archive --format=tar HEAD | tar -xf - -C "${source_dir}"' "${script}" >/dev/null
rg -F 'develop path:/work' "${script}" >/dev/null
rg -F 'go -C /work run ./cmd/control-plane release publish' "${script}" >/dev/null
rg -F -- '--env PLATFORM_STORE_URI' "${script}" >/dev/null
if rg -F 'source=${ROOT},target=/work' "${script}" >/dev/null ||
  rg -F -- '--privileged' "${script}" >/dev/null ||
  rg -F 'seccomp=unconfined' "${script}" >/dev/null; then
  printf 'not ok - materialized publisher escaped its bounded container contract\n' >&2
  exit 1
fi

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
repo="${tmp}/repo"
mkdir -p "${repo}/scripts" "${tmp}/bin"
cp "${script}" "${repo}/scripts/publish-materialized-platform-release.sh"
chmod +x "${repo}/scripts/publish-materialized-platform-release.sh"
printf 'tracked source\n' >"${repo}/tracked"
git init -q "${repo}"
git -C "${repo}" config user.email test@example.invalid
git -C "${repo}" config user.name test
git -C "${repo}" add scripts/publish-materialized-platform-release.sh tracked
git -C "${repo}" -c commit.gpgsign=false commit -qm initial

cat >"${tmp}/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
: >"${DOCKER_CALLED}"
source_dir=
publish_dir=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --mount)
      case "$2" in
        *,target=/work,readonly) source_dir="${2#*source=}"; source_dir="${source_dir%%,*}" ;;
        *,target=/input,readonly) publish_dir="${2#*source=}"; publish_dir="${publish_dir%%,*}" ;;
      esac
      shift 2
      ;;
    --env)
      printf 'env=%s\n' "$2" >>"${PUBLISH_LOG}"
      shift 2
      ;;
    *) shift ;;
  esac
done
[ -n "${source_dir}" ] && [ -n "${publish_dir}" ]
[ ! -e "${source_dir}/.git" ]
[ "$(cat "${source_dir}/tracked")" = 'tracked source' ]
mode() {
  if stat -c '%a' "$1" >/dev/null 2>&1; then
    stat -c '%a' "$1"
  else
    stat -f '%Lp' "$1"
  fi
}
printf 'source=%s\ninput=%s\nsource_mode=%s\ninput_mode=%s\n' \
  "${source_dir}" "${publish_dir}" "$(mode "${source_dir}")" "$(mode "${publish_dir}")" >>"${PUBLISH_LOG}"
while IFS= read -r -d '' file; do
  printf 'file=%s:%s\n' "${file#"${publish_dir}/"}" "$(mode "${file}")" >>"${PUBLISH_LOG}"
done < <(find "${publish_dir}" -type f -print0)
[ "${PLATFORM_STORE_URI}" = 's3://platform.example/releases' ]
[ "${AWS_ACCESS_KEY_ID}" = test-access ]
if [ "${DOCKER_FAIL:-0}" = 1 ]; then
  exit 19
fi
EOF
chmod +x "${tmp}/bin/docker"

input="${tmp}/release"
mkdir -p "${input}/objects/sha256"
printf '{}\n' >"${input}/platform-release.json"
printf 'runtime\n' >"${input}/objects/sha256/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
chmod 0644 "${input}/platform-release.json"
chmod 0444 "${input}/objects/sha256/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

run_publisher() {
  local publish_input=${4:-${input}}
  PATH="${tmp}/bin:${PATH}" \
    DOCKER_CALLED="$1" PUBLISH_LOG="$2" \
    AWS_ACCESS_KEY_ID=test-access AWS_SECRET_ACCESS_KEY=test-secret AWS_SESSION_TOKEN=test-session \
    AWS_REGION=us-west-2 AWS_DEFAULT_REGION=us-west-2 AWS_PROFILE=must-not-cross \
    "$3" s3://platform.example/releases "${publish_input}"
}

called="${tmp}/called"
log="${tmp}/publish.log"
run_publisher "${called}" "${log}" "${repo}/scripts/publish-materialized-platform-release.sh"
grep -Fxq 'source_mode=700' "${log}"
grep -Fxq 'input_mode=700' "${log}"
grep -Fxq 'file=platform-release.json:400' "${log}"
grep -Fxq 'file=objects/sha256/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:400' "${log}"
for env_name in AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN AWS_REGION AWS_DEFAULT_REGION PLATFORM_STORE_URI; do
  grep -Fxq "env=${env_name}" "${log}"
done
if grep -Fq 'AWS_PROFILE' "${log}"; then
  printf 'not ok - host AWS profile crossed the publisher boundary\n' >&2
  exit 1
fi
source_path="$(sed -n 's/^source=//p' "${log}")"
input_path="$(sed -n 's/^input=//p' "${log}")"
[ ! -e "${source_path}" ] && [ ! -e "${input_path}" ]

rm -f "${called}"
printf 'dirty\n' >"${repo}/dirty"
if run_publisher "${called}" "${tmp}/dirty.log" "${repo}/scripts/publish-materialized-platform-release.sh" \
  >"${tmp}/dirty.out" 2>"${tmp}/dirty.err"; then
  printf 'not ok - dirty Product checkout must fail closed\n' >&2
  exit 1
fi
grep -Fq 'requires a clean Product checkout' "${tmp}/dirty.err"
[ ! -e "${called}" ]
rm "${repo}/dirty"

mkdir "${tmp}/missing"
if run_publisher "${called}" "${tmp}/missing.log" \
  "${repo}/scripts/publish-materialized-platform-release.sh" "${tmp}/missing" \
  >"${tmp}/missing.out" 2>"${tmp}/missing.err"; then
  printf 'not ok - missing Platform manifest must fail closed\n' >&2
  exit 1
fi
grep -Fq 'manifest is missing' "${tmp}/missing.err"
[ ! -e "${called}" ]

failure_log="${tmp}/failure.log"
if DOCKER_FAIL=1 run_publisher "${called}" "${failure_log}" "${repo}/scripts/publish-materialized-platform-release.sh"; then
  printf 'not ok - publisher must propagate container failure\n' >&2
  exit 1
fi
failure_source="$(sed -n 's/^source=//p' "${failure_log}")"
failure_input="$(sed -n 's/^input=//p' "${failure_log}")"
[ ! -e "${failure_source}" ] && [ ! -e "${failure_input}" ]

git -C "${repo}" worktree add -q --detach "${tmp}/worktree"
git_dir="$(git -C "${tmp}/worktree" rev-parse --absolute-git-dir)"
mkfifo "${git_dir}/unsupported-entry"
run_publisher "${tmp}/worktree.called" "${tmp}/worktree.log" \
  "${tmp}/worktree/scripts/publish-materialized-platform-release.sh"

printf 'ok - materialized Platform release publisher contract\n'
