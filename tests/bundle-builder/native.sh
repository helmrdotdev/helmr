#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=tests/bundle-builder/common.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/common.sh"

# Native environment: build.builder installs an OS tool, copies and runs a
# setup script, node-gyp compiles an addon against a distribution library, a
# dependency ships a real postinstall executable, and a declaration module
# imports the native addon during analysis. The project also carries a
# .dockerignore naming files the build needs; it must have no effect.
native_project="$tmp/native-project"
cp -a "$repo_root/tests/fixtures/native-environment" "$native_project"
prepare_host_sdk "$native_project"
native_build() {
  local label="$1"
  "$shared/helmr" build "$native_project" --output "$tmp/$label" 2>"$tmp/$label.log" ||
    { tail -n 120 "$tmp/$label.log" >&2; return 1; }
  jq -e '.contract == "helmr.deployment-bundle.v0"' "$tmp/$label/bundle.json" >/dev/null
}
program_file() {
  local label="$1" path="$2" digest
  digest=$(jq -er '.program.artifact.digest | sub("^sha256:"; "")' "$tmp/$label/bundle.json")
  unsquashfs -cat "$tmp/$label/objects/sha256/$digest" "$path"
}
setup_step='RUN ["/bin/sh","/opt/native-environment/setup.sh"]'
packages_step='apt-get update && apt-get install -y --no-install-recommends jq'
input_copy_step='COPY [/recipe-input.txt,/etc/helmr-recipe-input]'
install_step='["npm","install","--no-audit","--no-fund"]'

native_build native
step_ran "$tmp/native.log" "$packages_step"
step_ran "$tmp/native.log" "$setup_step"
step_ran "$tmp/native.log" "$install_step"
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
step_cached "$tmp/native-unchanged.log" "$packages_step"
step_cached "$tmp/native-unchanged.log" "$setup_step"
step_cached "$tmp/native-unchanged.log" "$install_step"
[ "$(program_file native-unchanged generated/environment-stamp.txt)" = "$(program_file native generated/environment-stamp.txt)" ]
printf 'native cache: unchanged packages=hit setup=hit install=hit\n'

# A source file the lifecycle script reads: preparation is reused, the install
# is not, and the new bytes reach the Program.
echo "lifecycle input 2" >"$native_project/lifecycle-input.txt"
native_build native-lifecycle
step_cached "$tmp/native-lifecycle.log" "$packages_step"
step_cached "$tmp/native-lifecycle.log" "$setup_step"
step_cached "$tmp/native-lifecycle.log" "$input_copy_step"
step_ran "$tmp/native-lifecycle.log" "$install_step"
[ "$(program_file native-lifecycle generated/lifecycle.json)" = '{"input":"lifecycle input 2"}' ]
printf 'native cache: lifecycle packages=hit setup=hit input-copy=hit install=miss\n'

# A file the builder copies invalidates that step and the preparation after it.
echo "recipe input 2" >"$native_project/recipe-input.txt"
native_build native-recipe-input
step_cached "$tmp/native-recipe-input.log" "$packages_step"
step_ran "$tmp/native-recipe-input.log" "$input_copy_step"
step_ran "$tmp/native-recipe-input.log" "$setup_step"
printf 'native cache: recipe-input packages=hit input-copy=miss setup=miss\n'

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
"$shared/helmr" build "$shadow_project" --output "$tmp/shadow" 2>"$tmp/shadow.log" ||
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
if "$shared/helmr" build "$role_project" --output "$tmp/role" 2>"$tmp/role.log"; then
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
if "$shared/helmr" build "$bare_project" --output "$tmp/bare" 2>"$tmp/bare.log"; then
  echo "native fixture built without its OS preparation" >&2
  exit 1
fi
grep -F 'spawnSync jq ENOENT' "$tmp/bare.log" >/dev/null

HELMR_NATIVE_BUNDLE="$tmp/native-recipe-input" bash "$repo_root/tests/guestd_native_library_e2e.sh"

printf 'ok - bundle builder native lane\n'
