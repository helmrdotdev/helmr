#!/usr/bin/env bash
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
image=${HELMR_PREPARATION_BUILDER_IMAGE:-bundle-builder:0}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/source/agents" "$work/source/node_modules/@helmr" "$work/source/node_modules/@bufbuild"
for package in sdk proto; do
  cp -R "$repo_root/dist/npm/$package/package" "$work/source/node_modules/@helmr/$package"
done
cp -RL "$repo_root/sdk/typescript/node_modules/@bufbuild/protobuf" "$work/source/node_modules/@bufbuild/protobuf"
printf '%s' '{"type":"module","private":true,"dependencies":{"readable-tool":"1.0.0"}}' > "$work/source/package.json"
mkdir -p "$work/source/node_modules/readable-tool"
printf '%s' '{"name":"readable-tool","version":"1.0.0","type":"module","exports":"./index.js"}' > "$work/source/node_modules/readable-tool/package.json"
printf '%s' 'export const value="external-v1";' > "$work/source/node_modules/readable-tool/index.js"
printf '%s' 'asset-v1' > "$work/source/asset.txt"
printf '%s' 'export const value="shared-v1";' > "$work/source/shared.ts"
cat > "$work/source/agents/templates.ts" <<'TS'
import { template, image } from "@helmr/sdk"
import { value } from "../shared.ts"
import { readFile } from "node:fs/promises"
import { value as externalValue } from "readable-tool"
const base = image("base").from("node:24-bookworm-slim")
export const first = template({id:"first", image:base, resources:{cpu:1,memory:"1GiB"}, prepare:async()=>{if(!value || !externalValue)throw new Error("missing");await readFile("asset.txt")}})
export const second = template({id:"second", image:base, resources:{cpu:1,memory:"1GiB"}, prepare:async()=>{await readFile("asset.txt")}})
export const seed = template({id:"seed", image:base, resources:{cpu:1,memory:"1GiB"}})
TS
cat > "$work/source/agents/main.ts" <<'TS'
import { agent } from "@helmr/sdk"
import { first } from "./templates"
export const hello = agent({id:"hello",template:first,turn:()=>"agent-v1"})
TS
python3 - "$work" <<'PY'
import json, pathlib, shutil, sys
root=pathlib.Path(sys.argv[1])
for variant in ['baseline','agent-edit','dependency-edit','external-edit','nested','nested-agent-edit','missing-asset']:
 p=root/variant;p.mkdir();shutil.copytree(root/'source',p/'source')
 config=dict(dirs=['agents'],ignorePatterns=[],external=['readable-tool'],assets=[] if variant=='missing-asset' else ['asset.txt'])
 (p/'config.json').write_text(json.dumps(config,sort_keys=True,separators=(',',':')))
 if variant=='agent-edit':
  f=p/'source/agents/main.ts';f.write_text(f.read_text().replace('agent-v1','agent-v2'))
 if variant=='external-edit':
  f=p/'source/node_modules/readable-tool/index.js';f.write_text(f.read_text().replace('external-v1','external-v2'))
 if variant.startswith('nested'):
  (p/'source/agents/templates.ts').unlink()
  body='agent-v2' if variant=='nested-agent-edit' else 'agent-v1'
  (p/'source/agents/main.ts').write_text('import {agent,template,image} from "@helmr/sdk";import {readFile} from "node:fs/promises";export const hello=agent({id:"hello",template:template({id:"first",image:image("base").from("node:24-bookworm-slim"),resources:{cpu:1,memory:"1GiB"},prepare:async()=>{await readFile("asset.txt")}}),turn:()=>"'+body+'"});')
 if variant=='dependency-edit':
  f=p/'source/shared.ts';f.write_text(f.read_text().replace('shared-v1','shared-v2'))
PY
cat > "$work/prepare.sh" <<'SH'
set -eu
version=$(node -p process.versions.node)
for variant in baseline agent-edit dependency-edit external-edit nested nested-agent-edit missing-asset; do
 root=/fixture/$variant
 node /nix/helmr/program-compiler.mjs --bundle "$root/source" "$root/config.json" "$version" "$root/bundle"
 mkdir -p "$root/installed/node_modules" "$root/work"
 cp -R "$root/source/node_modules/readable-tool" "$root/installed/node_modules/readable-tool"
 printf '%s' '{"lockfileVersion":3,"packages":{"":{},"node_modules/readable-tool":{"version":"1.0.0","inBundle":true}}}' > "$root/installed/package-lock.json"
 node /nix/helmr/program-compiler.mjs --assemble "$root/bundle" "$root/installed" "$root/payload"
 /opt/helmr/bin/bundle-builder --project "$root/payload" --work "$root/work" --prepare-output "$root/prepared" \
  --runtime-descriptor /opt/helmr/release/runtime.descriptor.json --runtime-metadata /opt/helmr/runtime/helmr/runtime.json \
  --compiler-descriptor /nix/helmr/compiler.descriptor.json --node /opt/helmr/runtime/bin/node --config "$root/config.json" \
  --bundle-manifest "$root/bundle/bundle.json" --preparation-source "$root/source" --runtime-installed "$root/installed" \
  --program-compiler /nix/helmr/program-compiler.mjs
 done
# The independently analyzed image recipe must match the primary analysis before
# either recipe is replaced with its resolved seed descriptor.
cp -R /fixture/baseline/source /fixture/recipe-source
sed -i 's/node:24-bookworm-slim/node:24-bookworm/g' /fixture/recipe-source/agents/templates.ts
mkdir /fixture/recipe-work
if /opt/helmr/bin/bundle-builder --project /fixture/baseline/payload --work /fixture/recipe-work --prepare-output /fixture/recipe-prepared \
 --runtime-descriptor /opt/helmr/release/runtime.descriptor.json --runtime-metadata /opt/helmr/runtime/helmr/runtime.json \
 --compiler-descriptor /nix/helmr/compiler.descriptor.json --node /opt/helmr/runtime/bin/node --config /fixture/baseline/config.json \
 --bundle-manifest /fixture/baseline/bundle/bundle.json --preparation-source /fixture/recipe-source --runtime-installed /fixture/baseline/installed \
 --program-compiler /nix/helmr/program-compiler.mjs > /fixture/recipe.log 2>&1; then
 echo "mismatched preparation image recipe was accepted" >&2; exit 1
fi
grep -F 'independent preparation Template differs from Session analysis' /fixture/recipe.log
[ ! -e /fixture/recipe-prepared ]
# Runtime file reads are limited to the selected payload, including declared assets.
cd /fixture/baseline/prepared/preparations/first/payload
node --input-type=module -e 'const m=await import("./helmr/app/entry-0.mjs");await Object.values(m)[0].prepare({});'
cd /fixture/missing-asset/prepared/preparations/first/payload
node --input-type=module -e 'const m=await import("./helmr/app/entry-0.mjs");try{await Object.values(m)[0].prepare({});throw new Error("omitted asset remained readable")}catch(e){if(e.code!=="ENOENT")throw e;}'
SH
docker run --rm --platform linux/amd64 --user 0:0 --entrypoint /bin/bash \
 -v "$work:/fixture" "$image" /fixture/prepare.sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go -C "$repo_root" test -c -o "$work/builder.test" ./internal/builder
docker run --rm --platform linux/amd64 --user 0:0 --entrypoint /fixture/builder.test \
 -e TMPDIR=/tmp -e HELMR_PREPARATION_PROGRAM_FIXTURE=/fixture -v "$work:/fixture:ro" "$image" \
 -test.run '^TestIndependentPreparationPrograms$' -test.v -test.count=1
