#!/usr/bin/env bash
# Writes each `helmr init --template` project, points it at the local SDK build
# and typechecks it. Run after scripts/build-npm-packages.sh.
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

(cd "${repo_root}" && go build -o "${work}/helmr" ./cmd/helmr)
for starter in "${repo_root}"/starters/*/package.json; do
  template="$(basename "$(dirname "${starter}")")"
  project="${work}/${template}"
  echo "check starter ${template}"
  "${work}/helmr" init --dir "${project}" --template "${template}" >/dev/null
  SDK="${repo_root}/dist/npm/sdk/package" PROTO="${repo_root}/dist/npm/proto/package" bun -e '
    const path = process.argv[1] + "/package.json"
    const pkg = await Bun.file(path).json()
    pkg.dependencies["@helmr/sdk"] = "file:" + process.env.SDK
    pkg.overrides = { "@helmr/proto": "file:" + process.env.PROTO }
    await Bun.write(path, JSON.stringify(pkg, null, 2))
  ' "${project}"
  (cd "${project}" && bun install --ignore-scripts >/dev/null && bun run typecheck)
done
