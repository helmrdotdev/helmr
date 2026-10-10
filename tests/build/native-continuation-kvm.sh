#!/usr/bin/env bash
set -euo pipefail
# Invoked only after the retained dev scope and this execution are authorized.
# Arguments: absolute candidate directory, new absolute evidence directory.
candidate=${1:?candidate directory}
evidence=${2:?new evidence directory}
remote_store=${3:?isolated S3 prefix required}
mode=${4:-}
host_identity=${5:-}
continuation_object=${6:-}
case "$mode" in ""|source|restore) ;; *) echo 'mode must be source or restore' >&2; exit 1;; esac
if [[ -n "$mode" ]]; then [[ -n "$host_identity" ]]; fi
if [[ "$mode" == restore ]]; then [[ "$continuation_object" == /* && -f "$continuation_object" ]]; fi
[[ "$remote_store" == s3://*/_verification/native-continuation/* ]]
[[ $(id -u) == 0 && "$candidate" == /* && "$evidence" == /* ]]
[[ ! -e "$evidence" ]]
mkdir -m 0700 "$evidence"
[[ $(stat -c '%d' "$candidate") == $(stat -c '%d' "$evidence") ]] || { echo 'candidate and evidence must share a filesystem for sealed drive links' >&2; exit 1; }
cd "$candidate"
python3 - "$evidence" <<'PY_MANIFEST'
import hashlib,json,re,sys
from pathlib import Path
# Linux sockaddr_un reserves one byte for NUL. Include the complete jailer
# suffix and reject a long path before starting any host runtime work.
longest=str(Path(sys.argv[1])/'arena/jailer/firecracker'/('0'*36)/'root/vsock.sock')
if len(longest.encode())>107:raise SystemExit('evidence path is too long for Firecracker Unix sockets; use a short absolute directory')
required={'vmlinuz','initramfs','rootfs.squashfs','runtime-artifacts.json','agent-worker','agent-computerproof.test','source.tar','source.json','computer.ext4','runtime.squashfs','program.squashfs','model.mjs','runtime-image.txt','run.sh'}
manifest=Path('SHA256SUMS').read_bytes()
seen=set()
for line in manifest.decode().splitlines():
    digest,name=line.split(maxsplit=1)
    if not re.fullmatch('[0-9a-f]{64}',digest) or name not in required or name in seen:
        raise SystemExit('invalid or unexpected candidate manifest entry: '+name)
    seen.add(name)
if seen!=required: raise SystemExit('candidate manifest is incomplete')
evidence=Path(sys.argv[1])
(evidence/'SHA256SUMS').write_bytes(manifest)
(evidence/'manifest.sha256').write_text(hashlib.sha256(manifest).hexdigest()+'\n')
PY_MANIFEST
sha256sum --check SHA256SUMS > "$evidence/checksums.log"
python3 - "$evidence" <<'PY_SOURCE'
import hashlib,json,re,shutil,sys
from pathlib import Path
source=json.loads(Path('source.json').read_text())
if not re.fullmatch('[0-9a-f]{40}',source['product_commit']): raise SystemExit('invalid Product source commit')
for name,field in [('source.tar','source_archive_sha256'),('agent-computerproof.test','test_sha256'),('agent-worker','worker_sha256')]:
 h=hashlib.sha256()
 with open(name,'rb') as f:
  for block in iter(lambda:f.read(1024*1024),b''):h.update(block)
 if h.hexdigest()!=source[field]:raise SystemExit('source receipt does not bind '+name)
shutil.copyfile('source.json',Path(sys.argv[1])/'source.json')
PY_SOURCE
[[ -c /dev/kvm ]]
if getent passwd 61001 || getent group 61001 || pgrep -u 61001 >/dev/null; then
  echo 'fixture jailer UID/GID 61001 already assigned' >&2
  exit 1
fi
for device in nbd0 nbd1; do
  [[ -b /dev/$device ]]
  [[ ! -e /sys/block/$device/pid || -z $(cat /sys/block/$device/pid) ]]
  [[ $(cat /sys/block/$device/size) == 0 ]]
done
# An active Product service is an ownership conflict, not a request to stop it.
systemctl list-units --type=service --state=running,activating,reloading --no-legend > "$evidence/services.log"
if grep -E 'helmr.*(worker|dispatcher|control)' "$evidence/services.log" > "$evidence/conflicts.log"; then
  echo 'Product services are running; qualification not admitted' >&2
  exit 1
fi
inventory() {
  local phase=$1
  ip -4 -json route show table all > "$evidence/routes-$phase.json"
  ip -4 -json rule show > "$evidence/rules-$phase.json"
  ip netns list | sort > "$evidence/netns-$phase.log"
  findmnt -rn -o TARGET | sort > "$evidence/mounts-$phase.log"
  find /sys/fs/cgroup -type d -print | sort > "$evidence/cgroups-$phase.log"
  ps -eo pid=,comm= | awk '$2 ~ /^(firecracker|jailer|agent-worker|helmr-worker)$/ {print}' | sort > "$evidence/owners-$phase.log"
}
inventory before
ip -json link > "$evidence/links-before.json"
nft -json list tables > "$evidence/nft-before.json"
python3 - "$evidence/routes-before.json" <<'PY'
import ipaddress,json,sys
pools=[ipaddress.ip_network('198.18.32.0/24'),ipaddress.ip_network('198.19.32.0/24')]
for route in json.load(open(sys.argv[1])):
 d=route.get('dst','default')
 if d=='default':continue
 net=ipaddress.ip_network(d,strict=False)
 if any(net.overlaps(p) for p in pools):raise SystemExit('qualification address pool overlaps an existing route')
PY
mkdir -m 0700 "$evidence/source"
tar -xf source.tar -C "$evidence/source"
# Native pinned tools are selected from the exact candidate source. No profile
# installation, schema reset, host replacement, or application startup occurs.
cd "$evidence/source"
unset MKFS_EXT4_PATH MKE2FS_CONFIG_PATH FIRECRACKER_PATH JAILER_PATH WORKER_IMAGES_DIR
finish() {
  status=$?
  trap - EXIT
  set +e
  # The runtime removes exact owner groups; the fixture owns this shared parent
  # only when it did not exist at preflight. rmdir cannot remove a populated tree.
  if ! grep -Fxq /sys/fs/cgroup/firecracker "$evidence/cgroups-before.log" && [[ -d /sys/fs/cgroup/firecracker ]]; then
    rmdir /sys/fs/cgroup/firecracker || status=1
  fi
  inventory after || status=1
  ip -json link > "$evidence/links-after.json" || status=1
  nft -json list tables > "$evidence/nft-after.json" || status=1
  ps -eo pid,comm > "$evidence/processes-after.log" || status=1
  for device in nbd0 nbd1; do
    if [[ -e /sys/block/$device/pid && -n $(cat /sys/block/$device/pid) ]] || [[ $(cat /sys/block/$device/size) != 0 ]]; then
      printf '%s remains attached\n' "$device" >> "$evidence/cleanup-blockers.log"
      status=1
    fi
  done
  python3 - "$evidence" <<'PY_CHECK'
import json,re,sys
from pathlib import Path
p=Path(sys.argv[1])
for kind in ['routes','rules']:
 a=json.load(open(p/(kind+'-before.json')));b=json.load(open(p/(kind+'-after.json')))
 canonical=lambda rows: sorted(json.dumps(row,sort_keys=True) for row in rows)
 if canonical(a)!=canonical(b):raise SystemExit(kind+' changed; inspect fixture cleanup before host stop')
for kind in ['netns','owners']:
 if (p/(kind+'-before.log')).read_bytes()!=(p/(kind+'-after.log')).read_bytes():
  raise SystemExit(kind+' changed; inspect fixture cleanup before host stop')
log=(p/'kvm.log').read_text() if (p/'kvm.log').exists() else ''
instances=re.findall(r'owned VM instance: ([0-9a-f-]{36})',log)
for kind in ['mounts','cgroups']:
 before=set((p/(kind+'-before.log')).read_text().splitlines())
 after=set((p/(kind+'-after.log')).read_text().splitlines())
 changed=before^after
 owned=[x for x in changed if x.startswith(str(p)+'/') or any(i in x for i in instances) or kind=='cgroups' and '/firecracker' in x]
 if owned:
  (p/'cleanup-differences.log').write_text(kind+':\n'+'\n'.join(sorted(owned))+'\n')
  raise SystemExit('fixture '+kind+' changed; inspect before host stop')
for kind in ['links','nft']:
 a=json.load(open(p/(kind+'-before.json')))
 b=json.load(open(p/(kind+'-after.json')))
 if kind=='links':
  a={(x['ifindex'],x['ifname']) for x in a};b={(x['ifindex'],x['ifname']) for x in b}
 else:
  a={(x['table']['family'],x['table']['name']) for x in a['nftables'] if 'table' in x}
  b={(x['table']['family'],x['table']['name']) for x in b['nftables'] if 'table' in x}
 if a!=b:raise SystemExit(kind+' changed; inspect fixture cleanup before host stop')
PY_CHECK
  [[ $? == 0 ]] || status=1
  if [[ $status != 0 ]]; then
    printf '%s\n' 'Test or cleanup failed. Retain arena; inspect owned VMM/helper, cgroup, network and NBD state before cleanup or EC2 stop.' >> "$evidence/cleanup-blockers.log"
  fi
  exit "$status"
}
trap finish EXIT
nix develop .#smoke-linux -c bash -s -- "$candidate" "$evidence" "$remote_store" "$mode" "$host_identity" "$continuation_object" <<'INNER'
set -euo pipefail
candidate=$1
evidence=$2
remote_store=$3
mode=$4
host_identity=$5
continuation_object=$6
model_restore=()
if [[ "$mode" == restore ]]; then
  # Fetch the checkpoint metadata from the same remote namespace as the RAM and
  # disks. The operator carries only its digest/size descriptor between hosts.
  python3 - "$remote_store" "$continuation_object" "$evidence/continuation.json" <<'PY_FETCH'
import hashlib,json,re,subprocess,sys
from urllib.parse import urlparse
remote,receipt,output=sys.argv[1:]
obj=json.load(open(receipt)); uri=urlparse(remote)
digest=obj['Digest']
if not re.fullmatch(r'sha256:[0-9a-f]{64}',digest):raise SystemExit('invalid continuation digest')
key=uri.path.strip('/')+'/sha256/'+digest[7:]
if obj['Key']!=key or obj['MediaType']!='application/json' or not 0<obj['SizeBytes']<=16*1024*1024:raise SystemExit('invalid continuation object')
subprocess.run(['aws','s3api','get-object','--bucket',uri.netloc,'--key',key,output],check=True,stdout=subprocess.DEVNULL)
data=open(output,'rb').read()
if len(data)!=obj['SizeBytes'] or 'sha256:'+hashlib.sha256(data).hexdigest()!=digest:raise SystemExit('continuation bytes mismatch')
PY_FETCH
  model_restore=("$evidence/continuation.json")
fi
node "$candidate/model.mjs" "$evidence/model.json" 203.0.113.1 "${model_restore[@]}" > "$evidence/model.log" 2>&1 &
model_pid=$!
stop_model() { kill "$model_pid" 2>/dev/null || true; wait "$model_pid" || true; }
trap stop_model EXIT
for attempt in {1..200}; do
  kill -0 "$model_pid"
  [[ -s "$evidence/model.json" ]] && break
  sleep 0.05
done
[[ -s "$evidence/model.json" ]]
python3 - "$candidate" "$evidence" "$remote_store" "$mode" "$host_identity" <<'PY'
import json,os,shutil,sys
candidate,evidence,remote_store,mode,host_identity=sys.argv[1:]
def tool(name):
 p=shutil.which(name)
 if p is None:raise SystemExit('missing pinned tool: '+name)
 return p
runtime={
 'FirecrackerPath':tool('firecracker'),'CPUTemplateHelperPath':tool('cpu-template-helper'),
 'JailerPath':tool('jailer'),'JailerUID':61001,'JailerGID':61001,'CgroupVersion':'2',
 'KernelPath':candidate+'/vmlinuz','InitramfsPath':candidate+'/initramfs',
 'RootfsPath':candidate+'/rootfs.squashfs','RuntimeArtifactsPath':candidate+'/runtime-artifacts.json',
 'NetworkLinkPool':'198.18.32.0/24','NetworkTranslationPool':'198.19.32.0/24',
 'NetworkResolverIPv4':'1.1.1.1','NetworkCapacity':2,
 'IPPath':tool('ip'),'NFTPath':tool('nft'),
 'MkfsExt4Path':os.environ['MKFS_EXT4_PATH'],'Mke2fsConfigPath':os.environ['MKE2FS_CONFIG_PATH'],
 'VCPUCount':2,'MemoryMiB':2048,'ScratchDiskMiB':1024,
}
config={'Runtime':runtime,'Arena':evidence+'/arena','Helper':candidate+'/agent-worker',
        'Devices':['/dev/nbd0','/dev/nbd1'],'SeedDisk':candidate+'/computer.ext4',
        'RuntimeDisk':candidate+'/runtime.squashfs','ProgramDisk':candidate+'/program.squashfs',
        'ModelConfig':evidence+'/model.json','RemoteStore':remote_store,
        'Mode':mode,'HostIdentity':host_identity,'Continuation':evidence+'/continuation.json'}
with open(evidence+'/config.json','x') as f:json.dump(config,f,indent=2)
PY
export HELMR_DISPOSABLE_VM_PROOF=1
export HELMR_NATIVE_KVM_CONFIG="$evidence/config.json"
"$candidate/agent-computerproof.test" -test.run '^TestNativeComputerKVM$' -test.count=1 -test.timeout=12m -test.v > "$evidence/kvm.log" 2>&1
INNER
grep -E '^--- PASS: TestNativeComputerKVM ' "$evidence/kvm.log" >/dev/null
if grep -E '^\s*--- (SKIP|FAIL)' "$evidence/kvm.log"; then exit 1; fi
