#!/usr/bin/env bash
# Build a checksum-bound operator candidate from one clean local commit.
# Run inside nix develop .#images. No host, deployment or remote store is changed.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
output=${1:?new absolute candidate directory required}
[[ "$output" == /* ]]
cd "$repo_root"
[[ -z $(git status --porcelain) ]] || { echo 'candidate requires a clean source checkout' >&2; exit 1; }
commit=$(git rev-parse HEAD)
mkdir "$output"
tmp=$(mktemp -d)
mkdir -p "$repo_root/images/guest/out"
boot_tmp=$(mktemp -d "$repo_root/images/guest/out/native-boot.XXXXXX")
trap 'rm -rf "$tmp" "$boot_tmp"' EXIT
boot_relative="out/$(basename "$boot_tmp")"
"$repo_root/tests/build/native-continuation-artifacts.sh" "$tmp/artifacts"
make -C images/guest OUT="$boot_relative/out" GUESTD="$boot_relative/guestd"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 GOFLAGS='' GOEXPERIMENT='' GOTOOLCHAIN=local \
  go test -tags computerproof -c ./internal/firecracker -o "$output/agent-computerproof.test"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 GOFLAGS='' GOEXPERIMENT='' GOTOOLCHAIN=local \
  go build -trimpath -o "$output/agent-worker" ./cmd/worker
[[ $(git rev-parse HEAD) == "$commit" && -z $(git status --porcelain) ]] || { echo 'source changed during candidate build' >&2; exit 1; }
cp "$tmp/artifacts/"{runtime.squashfs,program.squashfs,computer.ext4,model.mjs,runtime-image.txt} "$output/"
cp "$boot_tmp/out/"{vmlinuz,initramfs,rootfs.squashfs,runtime-artifacts.json} "$output/"
cp tests/build/native-continuation-kvm.sh "$output/run.sh"
git archive --format=tar "$commit" > "$output/source.tar"
python3 - "$output" "$commit" <<'PY'
import hashlib,json,sys
from pathlib import Path
output=Path(sys.argv[1])
def digest(name):
 h=hashlib.sha256()
 with (output/name).open('rb') as f:
  for block in iter(lambda:f.read(1024*1024),b''):h.update(block)
 return h.hexdigest()
source={'product_commit':sys.argv[2], 'source_archive_sha256':digest('source.tar'),
        'test_sha256':digest('agent-computerproof.test'),'worker_sha256':digest('agent-worker'),
        'runtime_image':(output/'runtime-image.txt').read_text().strip()}
(output/'source.json').write_text(json.dumps(source,indent=2)+'\n')
names=sorted(p.name for p in output.iterdir())
(output/'SHA256SUMS').write_text(''.join(digest(n)+'  '+n+'\n' for n in names))
PY
# Remote extraction uses --no-same-owner as root. The immutable boot files must
# remain unreadable for writes by the unprivileged jailer.
chmod 0444 "$output/"{vmlinuz,initramfs,rootfs.squashfs}
printf 'candidate commit %s: %s\n' "$commit" "$output"
