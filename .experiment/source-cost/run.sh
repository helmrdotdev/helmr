#!/usr/bin/env bash
set -euo pipefail
repo=$(git rev-parse --show-toplevel)
source_dir="$repo/.experiment/source-cost"
out=${1:?provide a fresh absolute output directory}
case "$out" in /*) ;; *) echo 'output must be absolute' >&2; exit 1 ;; esac
if [[ -e "$out" ]]; then echo 'output must not exist' >&2; exit 1; fi
mkdir -p "$out/public-install" "$out/results"
cp "$source_dir/fixture/package.json" "$source_dir/fixture/bun.lock" "$out/public-install/"
scripts/build-compiler-entry.sh
scripts/build-npm-packages.sh
bun install --cwd "$out/public-install" --frozen-lockfile --ignore-scripts
python3 "$source_dir/prepare.py" "$repo" "$out"
chmod 777 "$out/results"
image=${SOURCE_COST_IMAGE:-node:24.21.0-bookworm-slim}
# Native architecture of the existing executor; never request amd64 emulation.
docker image inspect "$image" >"$out/image.json" 2>/dev/null || { docker pull "$image"; docker image inspect "$image" >"$out/image.json"; }
docker info --format '{{json .Architecture}}' >"$out/daemon-architecture.json"
python3 - "$out" <<'PYARCH'
from pathlib import Path
import json,sys
root=Path(sys.argv[1]);daemon=json.loads((root/'daemon-architecture.json').read_text());image=json.loads((root/'image.json').read_text())[0]['Architecture']
normalize=lambda value:{'aarch64':'arm64','x86_64':'amd64'}.get(value,value)
if normalize(daemon)!=normalize(image):raise SystemExit('benchmark requires native image architecture, not emulation')
PYARCH
git rev-parse HEAD >"$out/product-base.txt"
python3 - "$out" "$image" <<'PY'
from pathlib import Path
import subprocess,sys,json
root=Path(sys.argv[1]);image=sys.argv[2];commands=[]
for name,entry in json.loads((root/'workloads.json').read_text()).items():
 cmd=['docker','run','--rm','--network','none','--read-only','--user','65532:65532','--mount',f'type=bind,source={root},target=/bench,readonly','--mount',f'type=bind,source={root}/platform,target=/platform,readonly','--mount',f'type=bind,source={root}/workloads/{name},target=/project,readonly','--mount',f'type=bind,source={root}/results,target=/out','--entrypoint','node',image,'/bench/controller.mjs',entry,name]
 with (root/'results'/f'{name}.driver.log').open('w') as log:r=subprocess.run(cmd,stdout=log,stderr=subprocess.STDOUT)
 (root/'results'/f'{name}.driver.exit').write_text(str(r.returncode)+'\n');commands.append(dict(name=name,argv=cmd,exit=r.returncode));(root/'commands.json').write_text(json.dumps(commands,indent=2)+'\n')
 print(name,r.returncode,flush=True)
 if r.returncode:raise SystemExit(r.returncode)
PY

python3 "$source_dir/summarize.py" "$out"
