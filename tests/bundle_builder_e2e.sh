#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d)
registry_name="helmr-bundle-builder-registry-$$"
buildx_name="helmr-bundle-builder-$$"
cleanup() {
  docker buildx rm "$buildx_name" >/dev/null 2>&1 || true
  docker rm -f "$registry_name" >/dev/null 2>&1 || true
  if [ "${KEEP_BUNDLE_E2E_TMP:-0}" = 1 ]; then
    printf 'bundle builder e2e artifacts: %s\n' "$tmp" >&2
  else
    rm -rf "$tmp"
  fi
}
trap cleanup EXIT

docker run --detach --rm \
  --name "$registry_name" \
  --publish 127.0.0.1::5000 \
  "registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373" \
  >/dev/null
registry_port="$(docker port "$registry_name" 5000/tcp | sed -n 's/^127\.0\.0\.1://p')"
[ -n "$registry_port" ]
registry_endpoint="127.0.0.1:$registry_port"
if [ "$(uname -s)" = "Darwin" ]; then
  builder_registry_endpoint="host.docker.internal:$registry_port"
else
  builder_registry_endpoint="$registry_endpoint"
fi
for _ in $(seq 1 50); do
  if curl --fail --silent "http://$registry_endpoint/v2/" >/dev/null; then
    break
  fi
  sleep 0.2
done
curl --fail --silent "http://$registry_endpoint/v2/" >/dev/null

if [ -n "${BUNDLE_BUILDER_IMAGE_ARCHIVE:-}" ]; then
  [ -f "$BUNDLE_BUILDER_IMAGE_ARCHIVE" ]
  builder_archive="$BUNDLE_BUILDER_IMAGE_ARCHIVE"
else
  nix build "$repo_root#bundleBuilderImage" --out-link "$tmp/builder-image"
  builder_archive="$tmp/builder-image"
fi
docker load -i "$builder_archive" >/dev/null
printf '%s\n' '{"default":[{"type":"insecureAcceptAnything"}]}' >"$tmp/containers-policy.json"
skopeo --policy "$tmp/containers-policy.json" copy \
  --dest-tls-verify=false \
  docker-daemon:helmr/bundle-builder:0 \
  "docker://$registry_endpoint/helmr/bundle-builder:test" \
  >/dev/null
builder_digest="$(
  skopeo --policy "$tmp/containers-policy.json" inspect \
    --tls-verify=false \
    --format '{{.Digest}}' \
    "docker://$registry_endpoint/helmr/bundle-builder:test"
)"
[[ "$builder_digest" =~ ^sha256:[0-9a-f]{64}$ ]]
cat >"$tmp/buildkitd.toml" <<EOF
[registry."$builder_registry_endpoint"]
  http = true
  insecure = true
EOF
docker buildx create \
  --name "$buildx_name" \
  --driver docker-container \
  --driver-opt network=host \
  --buildkitd-config "$tmp/buildkitd.toml" \
  >/dev/null
export BUILDX_BUILDER="$buildx_name"
docker buildx inspect --bootstrap >/dev/null
builder_image="$builder_registry_endpoint/helmr/bundle-builder@$builder_digest"

go -C "$repo_root" build \
  -trimpath \
  -ldflags="-X main.deploymentBundleBuilderImage=$builder_image" \
  -o "$tmp/helmr" \
  ./cmd/helmr

project="$tmp/project"
mkdir -p "$project/tasks"
cat >"$project/prepare.sh" <<'SH'
#!/bin/sh
set -eu
mkdir -p node_modules/@helmr/sdk
cat >node_modules/@helmr/sdk/package.json <<'JSON'
{"name":"@helmr/sdk","type":"module"}
JSON
cat >node_modules/@helmr/sdk/index.js <<'JS'
const brand = Symbol.for("helmr.sdk.v0.definition")
export function defineConfig(config) { return config }
export function task(config) {
  return Object.freeze({
    [brand]: Object.freeze({
      kind: "task",
      id: config.id,
      hasPayload: false,
      handler: config.run,
    }),
  })
}
JS
SH
chmod 0755 "$project/prepare.sh"
cat >"$project/.yarnrc.yml" <<'YAML'
nodeLinker: node-modules
YAML
cat >"$project/helmr.config.ts" <<'TS'
import { defineConfig } from "@helmr/sdk"
export default defineConfig({ dirs: ["tasks"], ignorePatterns: [] })
TS
cat >"$project/tasks/hello.ts" <<'TS'
import { task } from "@helmr/sdk"
export const hello = task({ id: "hello", run: () => "hello" })
TS

write_package_json() {
  local selector="$1"
  if [ -n "$selector" ]; then
    jq -cn --arg selector "$selector" \
      '{name:"bundle-e2e",private:true,packageManager:$selector,scripts:{postinstall:"./prepare.sh"}}' \
      >"$project/package.json"
  else
    jq -cn \
      '{name:"bundle-e2e",private:true,scripts:{postinstall:"./prepare.sh"}}' \
      >"$project/package.json"
  fi
}

build_fixture() {
  local label="$1"
  local selector="$2"
  shift 2
  write_package_json "$selector"
  "$tmp/helmr" build "$project" --output "$tmp/$label" "$@"
  jq -e '.contract == "helmr.deployment-bundle.v0" and .workspaceImages == []' \
    "$tmp/$label/bundle.json" >/dev/null
}

build_fixture npm npm@11.5.1
build_fixture pnpm pnpm@10.14.0
build_fixture bun bun@1.3.10
build_fixture yarn yarn@4.9.2
build_fixture custom "" --install-command ./prepare.sh
export HELMR_E2E_BUILD_TOKEN="bundle-e2e-secret-sentinel"
build_secret_digest=$(printf '%s' "$HELMR_E2E_BUILD_TOKEN" | shasum -a 256 | awk '{print $1}')
build_fixture secret "" \
  --build-secret HELMR_E2E_BUILD_TOKEN \
  --install-command "test \"\$(sha256sum /run/secrets/HELMR_E2E_BUILD_TOKEN | cut -d' ' -f1)\" = '$build_secret_digest' && ./prepare.sh"
if rg -a -F "$HELMR_E2E_BUILD_TOKEN" "$tmp/secret"; then
  echo "build secret leaked into the deployment bundle" >&2
  exit 1
fi
unset HELMR_E2E_BUILD_TOKEN
build_fixture npm-repeat npm@11.5.1
diff -r "$tmp/npm" "$tmp/npm-repeat"

mutation_project="$tmp/mutation-project"
cp -a "$project" "$mutation_project"
cat >"$mutation_project/helmr.config.ts" <<'TS'
import { spawn } from "node:child_process"
import { existsSync, mkdirSync, writeFileSync } from "node:fs"
import { defineConfig } from "@helmr/sdk"
if (existsSync("/workspace/program")) {
  throw new Error("tenant code can observe the private Program assembly tree")
}
try {
  mkdirSync("/workspace/program")
  throw new Error("tenant code created the private Program assembly tree")
} catch (error) {
  const code = error && typeof error === "object" && "code" in error ? error.code : ""
  if (code !== "EACCES" && code !== "EROFS") {
    throw error
  }
}
try {
  writeFileSync("/workspace/project/mutation.txt", "must-not-be-written")
  throw new Error("installed tree was writable")
} catch (error) {
  const code = error && typeof error === "object" && "code" in error ? error.code : ""
  if (code !== "EACCES" && code !== "EROFS") {
    throw error
  }
}
const child = spawn(process.execPath, ["-e", `
  const { writeFileSync } = require("node:fs")
  setInterval(() => {
    try { writeFileSync("/workspace/program/detached-mutation.txt", "must-not-be-written") } catch {}
  }, 5)
`], { detached: true, stdio: "ignore" })
child.unref()
export default defineConfig({ dirs: ["tasks"], ignorePatterns: [] })
TS
"$tmp/helmr" build "$mutation_project" \
  --install-command ./prepare.sh \
  --output "$tmp/mutation-bundle" \
  >"$tmp/mutation.stdout" 2>"$tmp/mutation.stderr"
[ ! -e "$mutation_project/mutation.txt" ]
[ -f "$tmp/mutation-bundle/bundle.json" ]
mutation_program_digest=$(jq -er '.program.artifact.digest | sub("^sha256:"; "")' "$tmp/mutation-bundle/bundle.json")
mutation_program_listing=$(unsquashfs -ll "$tmp/mutation-bundle/objects/sha256/$mutation_program_digest")
if grep -F 'detached-mutation.txt' <<<"$mutation_program_listing"; then
  echo "detached tenant process mutated the finalized Program" >&2
  exit 1
fi

workspace_project="$tmp/workspace-project"
mkdir -p "$workspace_project/tasks"
cp "$project/helmr.config.ts" "$workspace_project/helmr.config.ts"
cat >"$workspace_project/package.json" <<'JSON'
{"name":"bundle-workspace-e2e","private":true}
JSON
cat >"$workspace_project/prepare.sh" <<'SH'
#!/bin/sh
set -eu
mkdir -p node_modules/@helmr/sdk
cat >node_modules/@helmr/sdk/package.json <<'JSON'
{"name":"@helmr/sdk","type":"module"}
JSON
cat >node_modules/@helmr/sdk/index.js <<JS
const brand = Symbol.for("helmr.sdk.v0.definition")
const sandboxBrand = Symbol.for("helmr.sdk.v0.sandbox")
export function defineConfig(config) { return config }
export function sandbox(config) {
  return Object.freeze({
    id: config.id,
    internal: Object.freeze({
      kind: "sandbox",
      id: config.id,
      image: Object.freeze({ key: "sandbox/" + config.id, steps: Object.freeze([{ kind: "from", ref: "$BASE_IMAGE" }]) }),
      resources: Object.freeze({ cpu: 1, memory: "1GiB" }),
    }),
    [sandboxBrand]: true,
  })
}
export const schedules = Object.freeze({
  task(config) {
    return Object.freeze({
      [brand]: Object.freeze({
        kind: "task",
        id: config.id,
        hasPayload: true,
        handler: config.run,
        schedule: Object.freeze({
          cron: config.cron.pattern,
          timezone: config.cron.timezone,
          workspace: Object.freeze({ sandbox: config.workspace.sandbox, secrets: Object.freeze([]) }),
        }),
      }),
    })
  },
})
JS
SH
chmod 0755 "$workspace_project/prepare.sh"
cat >"$workspace_project/tasks/hello.ts" <<'TS'
import { sandbox, schedules } from "@helmr/sdk"
export const machine = sandbox({ id: "machine" })
export const hello = schedules.task({
  id: "hello",
  cron: { pattern: "0 9 * * *", timezone: "UTC" },
  workspace: { sandbox: machine },
  run: () => "hello",
})
TS
"$tmp/helmr" build "$workspace_project" \
  --install-command "BASE_IMAGE=$builder_image ./prepare.sh" \
  --output "$tmp/workspace"
jq -e \
  '.contract == "helmr.deployment-bundle.v0" and (.workspaceImages | length) == 1 and .workspaceImages[0].declaredId == "machine"' \
  "$tmp/workspace/bundle.json" >/dev/null

printf 'ok - canonical bundle builder package-manager and workspace-image e2e\n'
