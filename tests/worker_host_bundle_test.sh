#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
host="${tmp}/host"
test_bin="${tmp}/bin"
mkdir -p "${host}/bin" "${test_bin}"

source_commit="$(git -C "${repo_root}" rev-parse HEAD)"
cat >"${host}/bin/helmr-worker" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
printf '#!/usr/bin/env bash\nexit 0\n' >"${host}/bin/cpu-template-helper"
printf '#!/usr/bin/env bash\nexit 0\n' >"${host}/bin/firecracker"
printf '#!/usr/bin/env bash\nexit 0\n' >"${host}/bin/jailer"
chmod 0755 "${host}/bin/"*
cat >"${test_bin}/file" <<'EOF'
#!/usr/bin/env bash
printf '%s: ELF 64-bit LSB executable, x86-64\n' "$1"
EOF
chmod 0755 "${test_bin}/file"

PATH="${test_bin}:${PATH}" "${repo_root}/scripts/materialize-worker-host-bundle.sh" "${tmp}/bundle-a" "${host}" >/dev/null
PATH="${test_bin}:${PATH}" "${repo_root}/scripts/materialize-worker-host-bundle.sh" "${tmp}/bundle-b" "${host}" >/dev/null

cmp "${tmp}/bundle-a/worker-host-artifacts.tar" "${tmp}/bundle-b/worker-host-artifacts.tar"
cmp "${tmp}/bundle-a/worker-host-artifacts.json" "${tmp}/bundle-b/worker-host-artifacts.json"
jq -e \
  --arg source_commit "${source_commit}" '
  (keys | sort) == ["bundle", "manifest", "schema", "sourceCommit"] and
  .schema == "helmr.worker-host-bundle.v0" and
  .sourceCommit == $source_commit and
  (.bundle | keys | sort) == ["digest", "path"] and
  .bundle.path == "worker-host-artifacts.tar" and
  (.bundle.digest | test("^sha256:[0-9a-f]{64}$")) and
  (.manifest | keys | sort) == ["digest", "path"] and
  .manifest.path == "worker-host-artifacts.json" and
  (.manifest.digest | test("^sha256:[0-9a-f]{64}$"))
' "${tmp}/bundle-a/worker-host-bundle.json" >/dev/null
jq -e '
  (keys | sort) == ["arch", "files", "schema"] and
  .schema == "helmr.worker-host-artifacts.v0" and
  .arch == "amd64" and
  [.files[].path] == ["cpu-template-helper", "firecracker", "helmr-worker", "jailer"] and
  all(.files[];
    .mode == "0755" and
    (.size_bytes | type == "number" and . > 0) and
    (.digest | test("^sha256:[0-9a-f]{64}$")))
' "${tmp}/bundle-a/worker-host-artifacts.json" >/dev/null

expected_entries="$(printf '%s\n' cpu-template-helper firecracker helmr-worker jailer worker-host-artifacts.json)"
[ "$(tar -tf "${tmp}/bundle-a/worker-host-artifacts.tar")" = "${expected_entries}" ]

mkdir "${tmp}/unpacked"
tar -C "${tmp}/unpacked" -xf "${tmp}/bundle-a/worker-host-artifacts.tar"
for name in cpu-template-helper firecracker helmr-worker jailer; do
  cmp "${host}/bin/${name}" "${tmp}/unpacked/${name}"
done
cmp "${tmp}/bundle-a/worker-host-artifacts.json" "${tmp}/unpacked/worker-host-artifacts.json"

printf 'ok - Worker host bundle tests\n'
