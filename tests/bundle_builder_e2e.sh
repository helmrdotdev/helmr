#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d)
registry_name="helmr-bundle-builder-registry-$$"
# shellcheck source=tests/buildx-fixture.sh
source "$repo_root/tests/buildx-fixture.sh"
# shellcheck source=tests/buildkit-steps.sh
source "$repo_root/tests/buildkit-steps.sh"
cleanup() {
  docker rm -f "$registry_name" >/dev/null 2>&1 || true
  cleanup_buildx_fixture
  if [ "${KEEP_BUNDLE_E2E_TMP:-0}" = 1 ]; then
    printf 'bundle builder e2e artifacts: %s\n' "$tmp" >&2
  else
    rm -rf "$tmp"
  fi
}
trap cleanup EXIT
start_buildx_fixture "$tmp"
buildx_name=$fixture_builder

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
[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.source"}}' bundle-builder:0)" = 'https://github.com/helmrdotdev/helmr' ]
# The builder is an ordinary Debian environment: standard ELF loader, native
# toolchain, and the Product-selected official Node as the only node on PATH.
docker run --rm --platform linux/amd64 --entrypoint node --workdir / \
  --volume "$repo_root:/product:ro" bundle-builder:0 --input-type=module -e '
    import { execFileSync } from "node:child_process"
    import { existsSync } from "node:fs"
    import assert from "node:assert/strict"
    import { requireVersion, managerInterpreter } from "/product/scripts/check-node-toolchain.mjs"
    requireVersion(process.versions.node, "builder image Node")
    assert.equal(process.execPath, "/usr/local/bin/node")
    assert.equal(existsSync("/usr/bin/node"), false, "distribution Node must not exist")
    assert.equal(existsSync("/lib64/ld-linux-x86-64.so.2"), true, "standard ELF loader")
    for (const command of ["npm", "npx", "corepack"]) {
      console.log(JSON.stringify(managerInterpreter(command)))
      execFileSync(command, ["--version"], {stdio:"inherit"})
    }
    for (const command of ["bun", "gcc", "g++", "make", "python3", "git", "pkg-config"]) {
      execFileSync(command, ["--version"], {stdio:"ignore"})
    }
    // Objects compiled here must load under the Runtime: its glibc may not be older.
    const version = text => text.match(/(\d+)\.(\d+)/).slice(1).map(Number)
    const builder = version(execFileSync("ldd", ["--version"], {encoding:"utf8"}).split("\n")[0].split(" ").at(-1))
    const runtime = version(execFileSync("/opt/helmr/runtime/lib/ld-linux-x86-64.so.2", ["--version"], {encoding:"utf8"}).split("\n")[0].match(/version (\S+)/)[1])
    assert.ok(builder[0] < runtime[0] || (builder[0] === runtime[0] && builder[1] <= runtime[1]), `builder glibc ${builder} exceeds Runtime glibc ${runtime}`)
    console.log(JSON.stringify({builderGlibc: builder.join("."), runtimeGlibc: runtime.join(".")}))
  '
printf '%s\n' '{"default":[{"type":"insecureAcceptAnything"}]}' >"$tmp/containers-policy.json"
skopeo --policy "$tmp/containers-policy.json" copy \
  --dest-tls-verify=false \
  docker-daemon:bundle-builder:0 \
  "docker://$registry_endpoint/bundle-builder:test" \
  >/dev/null
builder_digest="$(
  skopeo --policy "$tmp/containers-policy.json" inspect \
    --tls-verify=false \
    --format '{{.Digest}}' \
    "docker://$registry_endpoint/bundle-builder:test"
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
docker buildx inspect --bootstrap "$buildx_name" >/dev/null
builder_image="$builder_registry_endpoint/bundle-builder@$builder_digest"

go -C "$repo_root" build \
  -trimpath \
  -ldflags="-X main.deploymentBundleBuilderImage=$builder_image" \
  -o "$tmp/helmr" \
  ./cmd/helmr

# helmr.config.ts is evaluated once on this host, so a project prepares the
# packages its config imports here. The fixtures use the current packed SDK;
# host node_modules stay out of the captured source and out of the Program.
(cd "$repo_root" && bun install --frozen-lockfile --ignore-scripts >/dev/null && scripts/build-npm-packages.sh >/dev/null)
prepare_host_sdk() {
  local target="$1"
  mkdir -p "$target/node_modules/@helmr" "$target/node_modules/@bufbuild"
  cp -R "$repo_root/dist/npm/sdk/package" "$target/node_modules/@helmr/sdk"
  cp -R "$repo_root/dist/npm/proto/package" "$target/node_modules/@helmr/proto"
  cp -RL "$repo_root/sdk/typescript/node_modules/@bufbuild/protobuf" "$target/node_modules/@bufbuild/protobuf"
  grep -Fxq node_modules "$target/.helmrignore" 2>/dev/null || printf 'node_modules\n' >>"$target/.helmrignore"
}

project="$tmp/project"
mkdir -p "$project/tasks"
prepare_host_sdk "$project"
cp "$repo_root/internal/version/runtime-dependencies.json" "$project/runtime-dependencies.json"
cat >"$project/prepare.sh" <<'SH'
#!/bin/sh
set -eu
node -e '
  const expected = JSON.parse(require("node:fs").readFileSync("runtime-dependencies.json", "utf8")).node.version
  require("node:assert/strict").equal(process.versions.node, expected)
  console.log(JSON.stringify({surface:"install lifecycle", version:process.versions.node, execPath:process.execPath}))
'
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
# write_config takes the build settings as a TypeScript object literal.
write_config() {
  local target="$1" build="${2:-}"
  [ -n "$build" ] || build='{}'
  cat >"$target/helmr.config.ts" <<TS
import { defineConfig } from "@helmr/sdk"
export default defineConfig({ dirs: ["tasks"], ignorePatterns: [], build: $build })
TS
}
write_config "$project"
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
  "$tmp/helmr" build "$project" --output "$tmp/$label" 2>"$tmp/$label.log" ||
    { tail -n 80 "$tmp/$label.log" >&2; return 1; }
  jq -e '.contract == "helmr.deployment-bundle.v0" and .workspaceImages == []' \
    "$tmp/$label/bundle.json" >/dev/null
}

build_fixture npm npm@11.5.1
build_fixture pnpm pnpm@10.14.0
build_fixture bun bun@1.3.13
build_fixture yarn yarn@4.9.2
# An exact Bun selector is an ordinary versioned registry fetch.
grep -F '["npx","--yes","bun@1.3.13","install"]' "$tmp/bun.log" >/dev/null

# Without a selector the lockfile chooses the manager: Corepack's pinned pnpm
# and the image's official Bun. The lockfiles come from the same tools.
build_lockfile_fixture() {
  local label="$1" lockfile="$2" expected="$3"
  shift 3
  # A lockfile needs at least one package; the manifest stays selector-free.
  jq -cn '{name:"bundle-e2e",private:true,scripts:{postinstall:"./prepare.sh"},dependencies:{"is-number":"7.0.0"}}' \
    >"$project/package.json"
  docker run --rm --platform linux/amd64 --user "$(id -u):$(id -g)" \
    --env HOME=/tmp --env TMPDIR=/tmp --env XDG_CACHE_HOME=/tmp/cache \
    --volume "$project:/project" --workdir /project bundle-builder:0 "$@"
  [ -f "$project/$lockfile" ]
  "$tmp/helmr" build "$project" --output "$tmp/$label" 2>"$tmp/$label.log" ||
    { tail -n 80 "$tmp/$label.log" >&2; return 1; }
  jq -e '.contract == "helmr.deployment-bundle.v0"' "$tmp/$label/bundle.json" >/dev/null
  rm -f "${project:?}/$lockfile"
  grep -F "$expected" "$tmp/$label.log" >/dev/null
}
build_lockfile_fixture pnpm-lockfile pnpm-lock.yaml '["corepack","pnpm","install","--frozen-lockfile"]' \
  corepack pnpm install --lockfile-only --store-dir /tmp/pnpm-store
build_lockfile_fixture bun-lockfile bun.lock '["bun","install","--frozen-lockfile"]' \
  bun install --lockfile-only
# The install command and secret names are build settings in the config.
write_config "$project" '{ installCommand: "./prepare.sh" }'
build_fixture custom ""
grep -F '["/bin/bash","-euo","pipefail","-c","./prepare.sh"]' "$tmp/custom.log" >/dev/null
export HELMR_E2E_BUILD_TOKEN="bundle-e2e-secret-sentinel"
if command -v sha256sum >/dev/null 2>&1; then
  build_secret_digest=$(printf '%s' "$HELMR_E2E_BUILD_TOKEN" | sha256sum | awk '{print $1}')
else
  build_secret_digest=$(printf '%s' "$HELMR_E2E_BUILD_TOKEN" | shasum -a 256 | awk '{print $1}')
fi
cat >"$project/check-secret.sh" <<'SH'
#!/bin/sh
set -eu
test "$(sha256sum /run/secrets/HELMR_E2E_BUILD_TOKEN | cut -d' ' -f1)" = "$1"
SH
chmod 0755 "$project/check-secret.sh"
write_config "$project" "{ installCommand: \"./check-secret.sh $build_secret_digest && ./prepare.sh\", secrets: [\"HELMR_E2E_BUILD_TOKEN\"] }"
build_fixture secret ""
if rg -a -F "$HELMR_E2E_BUILD_TOKEN" "$tmp/secret" "$tmp/secret.log"; then
  echo "build secret leaked into the deployment bundle or build output" >&2
  exit 1
fi
rm "$project/check-secret.sh"
write_config "$project"
unset HELMR_E2E_BUILD_TOKEN
build_fixture npm-repeat npm@11.5.1
diff -r "$tmp/npm" "$tmp/npm-repeat"

mutation_project="$tmp/mutation-project"
cp -a "$project" "$mutation_project"
# Tenant isolation is a property of the target phases. The config now runs on
# the host, so the hostile module is a declaration module the target imports.
write_config "$mutation_project" '{ installCommand: "./prepare.sh" }'
cat >"$mutation_project/tasks/hello.ts" <<'TS'
import { spawn } from "node:child_process"
import { existsSync, mkdirSync, writeFileSync } from "node:fs"
import { task } from "@helmr/sdk"
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
export const hello = task({ id: "hello", run: () => "hello" })
TS
"$tmp/helmr" build "$mutation_project" \
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
prepare_host_sdk "$workspace_project"
write_config "$workspace_project" "{ installCommand: \"BASE_IMAGE=$builder_image ./prepare.sh\" }"
"$tmp/helmr" build "$workspace_project" \
  --output "$tmp/workspace"
jq -e \
  '.contract == "helmr.deployment-bundle.v0" and (.workspaceImages | length) == 1 and .workspaceImages[0].declaredId == "machine"' \
  "$tmp/workspace/bundle.json" >/dev/null

# Native environment: build.builder copies and runs a setup script that installs
# an OS tool, node-gyp compiles an addon against a distribution library, a
# dependency ships a real postinstall executable, and a declaration module
# imports the native addon during analysis. The project also carries a
# .dockerignore naming files the build needs; it must have no effect.
native_project="$tmp/native-project"
cp -a "$repo_root/tests/fixtures/native-environment" "$native_project"
prepare_host_sdk "$native_project"
native_build() {
  local label="$1"
  "$tmp/helmr" build "$native_project" --output "$tmp/$label" 2>"$tmp/$label.log" ||
    { tail -n 120 "$tmp/$label.log" >&2; return 1; }
  jq -e '.contract == "helmr.deployment-bundle.v0"' "$tmp/$label/bundle.json" >/dev/null
}
program_file() {
  local label="$1" path="$2" digest
  digest=$(jq -er '.program.artifact.digest | sub("^sha256:"; "")' "$tmp/$label/bundle.json")
  unsquashfs -cat "$tmp/$label/objects/sha256/$digest" "$path"
}
setup_step='RUN ["/bin/sh","/opt/native-environment/setup.sh"]'
input_copy_step='COPY [/recipe-input.txt,/etc/helmr-recipe-input]'
install_step='["npm","install","--no-audit","--no-fund"]'

native_build native
[ "$(program_file native generated/lifecycle.json)" = '{"input":"lifecycle input 1"}' ]
program_file native build/Release/distro_addon.node >/dev/null
# Preparation ran in exactly one graph; the install and the analysis, which
# compares the stamp the install recorded with the one it sees, shared it.
[ "$(awk '/^#0 building with /{ graph++ } { print graph "\t" $0 }' "$tmp/native.log" | grep -F -- "$setup_step" | cut -f1 | sort -u | wc -l | tr -d ' ')" = 1 ]
[[ "$(program_file native generated/environment-stamp.txt)" =~ ^[0-9]+$ ]]
# The host's node_modules, prepared only so the config could be evaluated, are
# not part of the Program: the target installed its own.
if program_file native node_modules/@bufbuild/protobuf/package.json >/dev/null 2>&1; then
  echo "host node_modules reached the Program" >&2
  exit 1
fi

native_build native-unchanged
step_cached "$tmp/native-unchanged.log" "$setup_step"
step_cached "$tmp/native-unchanged.log" "$install_step"
[ "$(program_file native-unchanged generated/environment-stamp.txt)" = "$(program_file native generated/environment-stamp.txt)" ]

# A source file the lifecycle script reads: preparation is reused, the install
# is not, and the new bytes reach the Program.
echo "lifecycle input 2" >"$native_project/lifecycle-input.txt"
native_build native-lifecycle
step_cached "$tmp/native-lifecycle.log" "$setup_step"
step_cached "$tmp/native-lifecycle.log" "$input_copy_step"
step_ran "$tmp/native-lifecycle.log" "$install_step"
[ "$(program_file native-lifecycle generated/lifecycle.json)" = '{"input":"lifecycle input 2"}' ]

# A file the builder copies invalidates that step and the preparation after it.
echo "recipe input 2" >"$native_project/recipe-input.txt"
native_build native-recipe-input
step_ran "$tmp/native-recipe-input.log" "$input_copy_step"
step_ran "$tmp/native-recipe-input.log" "$setup_step"

# A builder step that replaces Helmr's tools and plants stale files beside them
# does not change which compiler, Runtime and builder evaluate the project.
shadow_project="$tmp/shadow-project"
cp -a "$native_project" "$shadow_project"
cat >"$shadow_project/environment/shadow.sh" <<'SH'
#!/bin/sh
set -eu
rm -rf /opt/helmr/bin /opt/helmr/runtime /nix/helmr
mkdir -p /opt/helmr/bin /nix/helmr
printf '#!/bin/sh\necho shadowed-tool >&2\nexit 97\n' >/opt/helmr/bin/bundle-builder
chmod 0755 /opt/helmr/bin/bundle-builder
ln -s /bin/false /opt/helmr/bin/mksquashfs
echo stale >/nix/helmr/program-compiler.mjs
SH
sed -i.bak 's|      .run(\["/bin/sh", "/opt/native-environment/setup.sh"\]),|      .run(["/bin/sh", "/opt/native-environment/setup.sh"])\
      .copy("environment/shadow.sh", "/opt/native-environment/shadow.sh")\
      .run(["/bin/sh", "/opt/native-environment/shadow.sh"]),|' "$shadow_project/helmr.config.ts"
rm "$shadow_project/helmr.config.ts.bak"
grep -F 'shadow.sh' "$shadow_project/helmr.config.ts" >/dev/null
"$tmp/helmr" build "$shadow_project" --output "$tmp/shadow" 2>"$tmp/shadow.log" ||
  { tail -n 120 "$tmp/shadow.log" >&2; exit 1; }
# Output of a step is "#N <seconds> <text>"; the instruction text itself is
# not evidence of execution.
if grep -E '^#[0-9]+ [0-9.]+ shadowed-tool' "$tmp/shadow.log"; then
  echo "a tool provided by the build environment was executed" >&2
  exit 1
fi
[ "$(jq -c '[.platform, .runtime]' "$tmp/shadow/bundle.json")" = "$(jq -c '[.platform, .runtime]' "$tmp/native-recipe-input/bundle.json")" ]

# The build environment is its own role: a Workspace image is rejected while
# the config is evaluated, before any Linux work starts.
role_project="$tmp/role-project"
cp -a "$native_project" "$role_project"
cat >"$role_project/helmr.config.ts" <<'TS'
import { defineConfig, image } from "@helmr/sdk"
export default defineConfig({ dirs: ["tasks"], build: { builder: image("not-a-builder").from("ubuntu:24.04") as never } })
TS
if "$tmp/helmr" build "$role_project" --output "$tmp/role" 2>"$tmp/role.log"; then
  echo "a Workspace image was accepted as the build environment" >&2
  exit 1
fi
grep -F 'must be created by builder()' "$tmp/role.log" >/dev/null
if grep -F '#0 building with' "$tmp/role.log"; then
  echo "an invalid config reached BuildKit" >&2
  exit 1
fi

# Without build.builder the tool the postinstall needs is absent from the
# builder image and the ordinary lifecycle error surfaces; nothing papers over it.
bare_project="$tmp/bare-project"
cp -a "$native_project" "$bare_project"
write_config "$bare_project"
if "$tmp/helmr" build "$bare_project" --output "$tmp/bare" 2>"$tmp/bare.log"; then
  echo "native fixture built without its OS preparation" >&2
  exit 1
fi
grep -F 'spawnSync jq ENOENT' "$tmp/bare.log" >/dev/null

HELMR_NATIVE_BUNDLE="$tmp/native-recipe-input" bash "$repo_root/tests/guestd_native_library_e2e.sh"

# Agent tool work: the real SDK declares the tasks and a Workspace image with a
# browser, Git and Python; the Program carries Playwright and Sharp.
agentic_project="$tmp/agentic-project"
cp -a "$repo_root/tests/fixtures/agentic-work" "$agentic_project"
prepare_host_sdk "$agentic_project"
"$tmp/helmr" build "$agentic_project" --output "$tmp/agentic" 2>"$tmp/agentic.log" ||
  { tail -n 120 "$tmp/agentic.log" >&2; exit 1; }
jq -e '[.workspaceImages[].declaredId] == ["agentic-work"]' "$tmp/agentic/bundle.json" >/dev/null
HELMR_AGENTIC_BUNDLE="$tmp/agentic" bash "$repo_root/tests/agentic_work_e2e.sh"

printf 'ok - canonical bundle builder package-manager, workspace-image, native-environment and agentic-work e2e\n'
