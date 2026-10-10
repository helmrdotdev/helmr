#!/usr/bin/env bash
# Execute only after authorization for this disposable host and S3 prefix.
set -euo pipefail
candidate=${1:?absolute candidate directory}
evidence=${2:?new short absolute evidence directory}
remote=${3:?isolated S3 prefix}
host_id=${4:?verified EC2 instance ID}
role=${5:-source}
bootstrap=${6:-}
[[ "$role" == source || "$role" == target ]]
if [[ "$role" == source ]]; then
  : "${HELMR_TEST_DATABASE_URL:?explicit disposable PostgreSQL admin URL}"
else
  [[ "$bootstrap" == /* && ! -e "$bootstrap" && ! -L "$bootstrap" ]]
fi
: "${AWS_REGION:?explicit AWS region}"
test_case=TestWorkerAgentNativeExecution
if [[ -n ${HELMR_NATIVE_CROSS_HOST_TARGET_ID:-} ]]; then
  [[ "$role" == source && "$HELMR_NATIVE_CROSS_HOST_TARGET_ID" != "$host_id" ]]
  [[ ${HELMR_NATIVE_MISSING_CHECKPOINT_MEMORY_PROOF:-0} != 1 && ${HELMR_NATIVE_CORRUPT_CHECKPOINT_PROOF:-0} != 1 && ${HELMR_NATIVE_HEALTHY_RAM_PROOF:-0} != 1 && ${HELMR_NATIVE_PROCESS_LOSS_PROOF:-0} != 1 && ${HELMR_NATIVE_AUTHORITY_LOSS_PROOF:-0} != 1 && ${HELMR_NATIVE_NETWORK_RECOVERY_PROOF:-0} != 1 && ${HELMR_NATIVE_RESTART_PUBLICATION_OWNER:-0} != 1 ]]
  test_case=TestWorkerAgentCrossHostContinuation
fi
if [[ ${HELMR_NATIVE_MISSING_CHECKPOINT_MEMORY_PROOF:-0} == 1 ]]; then
  [[ ${HELMR_NATIVE_CORRUPT_CHECKPOINT_PROOF:-0} != 1 && ${HELMR_NATIVE_HEALTHY_RAM_PROOF:-0} != 1 && ${HELMR_NATIVE_PROCESS_LOSS_PROOF:-0} != 1 && ${HELMR_NATIVE_AUTHORITY_LOSS_PROOF:-0} != 1 && ${HELMR_NATIVE_NETWORK_RECOVERY_PROOF:-0} != 1 && ${HELMR_NATIVE_RESTART_PUBLICATION_OWNER:-0} != 1 ]]
  test_case=TestWorkerAgentMissingCheckpointMemory
fi
if [[ ${HELMR_NATIVE_CORRUPT_CHECKPOINT_PROOF:-0} == 1 ]]; then
  [[ ${HELMR_NATIVE_HEALTHY_RAM_PROOF:-0} != 1 && ${HELMR_NATIVE_PROCESS_LOSS_PROOF:-0} != 1 && ${HELMR_NATIVE_AUTHORITY_LOSS_PROOF:-0} != 1 && ${HELMR_NATIVE_NETWORK_RECOVERY_PROOF:-0} != 1 && ${HELMR_NATIVE_RESTART_PUBLICATION_OWNER:-0} != 1 ]]
  test_case=TestWorkerAgentCorruptCheckpoint
fi
if [[ ${HELMR_NATIVE_HEALTHY_RAM_PROOF:-0} == 1 ]]; then
  [[ ${HELMR_NATIVE_PROCESS_LOSS_PROOF:-0} != 1 && ${HELMR_NATIVE_AUTHORITY_LOSS_PROOF:-0} != 1 && ${HELMR_NATIVE_NETWORK_RECOVERY_PROOF:-0} != 1 && ${HELMR_NATIVE_RESTART_PUBLICATION_OWNER:-0} != 1 ]]
  test_case=TestWorkerAgentHealthyContinuation
fi
if [[ ${HELMR_NATIVE_PROCESS_LOSS_PROOF:-0} == 1 ]]; then
  [[ ${HELMR_NATIVE_AUTHORITY_LOSS_PROOF:-0} != 1 && ${HELMR_NATIVE_NETWORK_RECOVERY_PROOF:-0} != 1 && ${HELMR_NATIVE_RESTART_PUBLICATION_OWNER:-0} != 1 ]]
  test_case=TestWorkerAgentNativeProcessLoss
fi
if [[ ${HELMR_NATIVE_AUTHORITY_LOSS_PROOF:-0} == 1 ]]; then
  [[ ${HELMR_NATIVE_RESTART_PUBLICATION_OWNER:-0} != 1 ]]
  test_case=TestWorkerAgentAuthorityLoss
  if [[ ${HELMR_NATIVE_NETWORK_RECOVERY_PROOF:-0} == 1 ]]; then
    test_case=TestWorkerAgentRetainedNetworkRecovery
  fi
fi
if [[ ${HELMR_NATIVE_LONG_IDLE_PROOF:-0} == 1 ]]; then
  [[ "$role" == source && "$test_case" == TestWorkerAgentNativeExecution && ${HELMR_NATIVE_NETWORK_RECOVERY_PROOF:-0} != 1 && ${HELMR_NATIVE_RESTART_PUBLICATION_OWNER:-0} != 1 ]]
  test_case=TestWorkerAgentLongIdleContinuation
fi
if [[ ${HELMR_NATIVE_CHECKPOINT_KEY_MISMATCH_PROOF:-0} == 1 ]]; then
  [[ "$role" == source && "$test_case" == TestWorkerAgentNativeExecution && ${HELMR_NATIVE_NETWORK_RECOVERY_PROOF:-0} != 1 && ${HELMR_NATIVE_RESTART_PUBLICATION_OWNER:-0} != 1 ]]
  test_case=TestWorkerAgentCheckpointKeyMismatch
fi
if [[ ${HELMR_NATIVE_FINALIZATION_CANCEL_PROOF:-0} == 1 ]]; then
  [[ "$role" == source && "$test_case" == TestWorkerAgentNativeExecution && ${HELMR_NATIVE_NETWORK_RECOVERY_PROOF:-0} != 1 && ${HELMR_NATIVE_RESTART_PUBLICATION_OWNER:-0} != 1 ]]
  test_case=TestWorkerAgentFinalizationCancellation
fi
if [[ ${HELMR_NATIVE_FINALIZATION_DEADLINE_PROOF:-0} == 1 ]]; then
  [[ "$role" == source && "$test_case" == TestWorkerAgentNativeExecution && ${HELMR_NATIVE_NETWORK_RECOVERY_PROOF:-0} != 1 && ${HELMR_NATIVE_RESTART_PUBLICATION_OWNER:-0} != 1 ]]
  test_case=TestWorkerAgentFinalizationDeadline
fi
if [[ ${HELMR_NATIVE_BACKGROUND_SAVE_PROOF:-0} == 1 ]]; then
  [[ "$role" == source && "$test_case" == TestWorkerAgentNativeExecution && ${HELMR_NATIVE_NETWORK_RECOVERY_PROOF:-0} != 1 ]]
  test_case=TestWorkerAgentBackgroundSaves
fi
if [[ ${HELMR_NATIVE_CHECKPOINT_READ_RETRY_PROOF:-0} == 1 ]]; then
  [[ "$role" == source && "$test_case" == TestWorkerAgentNativeExecution && ${HELMR_NATIVE_NETWORK_RECOVERY_PROOF:-0} != 1 && ${HELMR_NATIVE_RESTART_PUBLICATION_OWNER:-0} != 1 ]]
  test_case=TestWorkerAgentCheckpointReadRetry
fi
if [[ ${HELMR_NATIVE_SESSION_DISCOVERY_PROOF:-0} == 1 ]]; then
  [[ "$role" == source && "$test_case" == TestWorkerAgentNativeExecution && ${HELMR_NATIVE_NETWORK_RECOVERY_PROOF:-0} != 1 && ${HELMR_NATIVE_RESTART_PUBLICATION_OWNER:-0} != 1 ]]
  test_case=TestWorkerAgentSessionDiscoveryContinuation
fi
if [[ ${HELMR_NATIVE_SECRET_BINDINGS_PROOF:-0} == 1 ]]; then
  [[ "$role" == source && "$test_case" == TestWorkerAgentNativeExecution && ${HELMR_NATIVE_NETWORK_RECOVERY_PROOF:-0} != 1 && ${HELMR_NATIVE_RESTART_PUBLICATION_OWNER:-0} != 1 ]]
  test_case=TestWorkerAgentSecretBindingsContinuation
fi
if [[ -n ${HELMR_NATIVE_PERFORMANCE_KIND:-} ]]; then
  [[ "$role" == source && "$test_case" == TestWorkerAgentNativeExecution && ${HELMR_NATIVE_RESTART_PUBLICATION_OWNER:-0} != 1 ]]
  [[ "$HELMR_NATIVE_PERFORMANCE_KIND" == no-op || "$HELMR_NATIVE_PERFORMANCE_KIND" == edit-test || "$HELMR_NATIVE_PERFORMANCE_KIND" == dependencies ]]
  [[ ${HELMR_NATIVE_PERFORMANCE_DEGRADED:-0} == 0 || ${HELMR_NATIVE_PERFORMANCE_DEGRADED:-0} == 1 ]]
  [[ ${HELMR_NATIVE_PERFORMANCE_BACKGROUND:-0} == 0 || ${HELMR_NATIVE_PERFORMANCE_BACKGROUND:-0} == 1 ]]
  test_case=TestWorkerAgentPerformance
fi
[[ $(id -u) == 0 && $(uname -m) == x86_64 && "$candidate" == /* && "$evidence" == /* ]]
[[ "$remote" == s3://*/_verification/native-execution/* && "$remote" != *'?'* && "$remote" != *'#'* ]]
[[ $(tr -d '\n' </sys/devices/virtual/dmi/id/board_asset_tag) == "$host_id" ]]
[[ -c /dev/kvm && ! -e "$evidence" ]]
getent passwd helmr-verifier >/dev/null
getent group helmr-verifier >/dev/null
if getent passwd 61001 || getent group 61001 || pgrep -u 61001 >/dev/null; then
  echo 'fixture jailer UID/GID already assigned' >&2; exit 1
fi
mkdir -m 0700 "$evidence"
cd "$candidate"
python3 - "$candidate" "$evidence" <<'PY'
import hashlib,json,re,sys
from pathlib import Path
p,e=map(Path,sys.argv[1:])
if len(str(e/'w/jailer/firecracker'/('0'*36)/'root/vsock.sock').encode())>107:raise SystemExit('evidence path exceeds Unix socket limit')
seen=set()
for line in (p/'SHA256SUMS').read_text().splitlines():
 digest,name=line.split('  ',1); rel=Path(name)
 if not re.fullmatch('[0-9a-f]{64}',digest) or rel.is_absolute() or '..' in rel.parts or name in seen:raise SystemExit('invalid manifest')
 path=p/rel
 if any(x.is_symlink() for x in [path,*path.parents]):raise SystemExit('candidate symlink')
 h=hashlib.sha256()
 with path.open('rb') as f:
  for b in iter(lambda:f.read(1024*1024),b''):h.update(b)
 if h.hexdigest()!=digest:raise SystemExit('candidate digest mismatch: '+name)
 seen.add(name)
required={'agent-worker','agent-execution.test','nbd-recovery.test','driver.mjs','model.mjs','runtime.squashfs','runtime.descriptor.json','source.json','source.tar','run.sh','bundle/bundle.json','images/guest/out/vmlinuz','images/guest/out/initramfs','images/guest/out/rootfs.squashfs','images/guest/out/runtime-artifacts.json'}
if not required<=seen:raise SystemExit('incomplete candidate')
s=json.loads((p/'source.json').read_text())
if not re.fullmatch('[0-9a-f]{40}',s['product_commit']):raise SystemExit('invalid source identity')
for name,key in [('source.tar','source_archive_sha256'),('agent-worker','worker_sha256'),('agent-execution.test','test_sha256'),('nbd-recovery.test','nbd_test_sha256')]:
 h=hashlib.sha256((p/name).read_bytes()).hexdigest()
 if h!=s[key]:raise SystemExit('source receipt mismatch')
(e/'source.json').write_bytes((p/'source.json').read_bytes())
(e/'SHA256SUMS').write_bytes((p/'SHA256SUMS').read_bytes())
PY
[[ $(stat -c '%d' "$candidate") == $(stat -c '%d' "$evidence") ]]
for device in nbd0 nbd1; do
  [[ -b /dev/$device && $(cat /sys/block/$device/size) == 0 ]]
  [[ ! -e /sys/block/$device/pid || -z $(cat /sys/block/$device/pid) ]]
done
systemctl list-units --type=service --state=running,activating,reloading --no-legend > "$evidence/services.log"
if grep -E 'helmr.*(worker|dispatcher|control)' "$evidence/services.log"; then echo 'Product service conflict' >&2; exit 1; fi
group="/sys/fs/cgroup/helmr-native-execution-$$"
[[ ! -e "$group" ]]
inventory() {
  local phase=$1
  ip -4 -json route show table all > "$evidence/routes-$phase.json"
  ip -4 -json rule show > "$evidence/rules-$phase.json"
  ip -json link > "$evidence/links-$phase.json"
  nft -json list tables > "$evidence/nft-$phase.json"
  ip netns list | sort > "$evidence/netns-$phase.log"
  findmnt -rn -o TARGET | sort > "$evidence/mounts-$phase.log"
  { for tree in "$group" /sys/fs/cgroup/firecracker; do
      if [[ -d "$tree" ]]; then find "$tree" -type d -print; fi
    done; } | sort > "$evidence/cgroups-$phase.log"
  ps -eo pid=,comm= | awk '$2 ~ /^(firecracker|jailer|agent-worker)$/ {print}' | sort > "$evidence/owners-$phase.log"
}
inventory before
python3 - "$evidence/routes-before.json" <<'PY'
import ipaddress,json,sys
pools=[ipaddress.ip_network('198.18.32.0/24'),ipaddress.ip_network('198.19.32.0/24')]
for r in json.load(open(sys.argv[1])):
 if r.get('dst','default')=='default':continue
 if any(ipaddress.ip_network(r['dst'],strict=False).overlaps(p) for p in pools):raise SystemExit('fixture network pool occupied')
PY
# The production worker requires a process-free delegated parent and supervisor
# leaf. Only this newly created subtree is owned by the fixture.
for controller in cpu memory pids; do grep -qw "$controller" /sys/fs/cgroup/cgroup.subtree_control; done
mkdir "$group"
finish() {
  status=$?
  trap - EXIT
  set +e
  rmdir "$group/supervisor" "$group" || status=1
  if ! grep -Fxq /sys/fs/cgroup/firecracker "$evidence/cgroups-before.log" && [[ -d /sys/fs/cgroup/firecracker ]]; then rmdir /sys/fs/cgroup/firecracker || status=1; fi
  # ip netns creates a shared parent bind mount on its first use. Remove it
  # only when this exclusive fixture created it and all namespace entries left.
  if ! grep -Fxq /run/netns "$evidence/mounts-before.log" && mountpoint -q /run/netns; then
    if [[ -z $(ip netns list) && -z $(find /run/netns -mindepth 1 -maxdepth 1 -print -quit) ]]; then
      umount /run/netns || status=1
    else status=1; fi
  fi
  inventory after || status=1
  for device in nbd0 nbd1; do
    if [[ $(cat /sys/block/$device/size) != 0 ]] || [[ -e /sys/block/$device/pid && -n $(cat /sys/block/$device/pid) ]]; then status=1; fi
  done
  if python3 - "$evidence" <<'PY'
import json,sys
from pathlib import Path
p=Path(sys.argv[1])
for kind in ['routes','rules','links','nft']:
 a=json.loads((p/(kind+'-before.json')).read_text());b=json.loads((p/(kind+'-after.json')).read_text())
 if kind=='links':
  a={(x['ifindex'],x['ifname']) for x in a};b={(x['ifindex'],x['ifname']) for x in b}
 elif kind=='nft':
  a={(x['table']['family'],x['table']['name']) for x in a['nftables'] if 'table' in x};b={(x['table']['family'],x['table']['name']) for x in b['nftables'] if 'table' in x}
 else:
  a=sorted(json.dumps(x,sort_keys=True) for x in a);b=sorted(json.dumps(x,sort_keys=True) for x in b)
 if a!=b:raise SystemExit(kind+' changed: operator cleanup audit required')
for kind in ['netns','owners','mounts','cgroups']:
 a=(p/(kind+'-before.log')).read_text();b=(p/(kind+'-after.log')).read_text()
 if kind=='mounts':
  # Login runtime roots follow SSH session lifetime, independently of this
  # system service. Retain raw inventories; still check every other mount.
  import re
  a=[line for line in a.splitlines() if not re.fullmatch(r'/run/user/[0-9]+',line)]
  b=[line for line in b.splitlines() if not re.fullmatch(r'/run/user/[0-9]+',line)]
 if a!=b:raise SystemExit(kind+' changed: operator cleanup audit required')
PY
  then :; else status=1; fi
  if [[ $status != 0 ]]; then echo 'Retain evidence and working state; inspect physical owners, NBD, mounts, cgroups and network before cleanup or host stop.' > "$evidence/cleanup-blockers.log"; fi
  exit "$status"
}
trap finish EXIT
printf '%s' '+cpu +memory +pids' > "$group/cgroup.subtree_control"
mkdir "$group/supervisor"
if [[ ${HELMR_NATIVE_AUTHORITY_LOSS_PROOF:-0} == 1 ]]; then
  for number in 0 1; do
    HELMR_NBD_RECOVERY_DEVICE="/dev/nbd$number" "$candidate/nbd-recovery.test" \
      -test.run '^TestIdleDeviceKernelProbe$' -test.v -test.count=1 -test.timeout=30s > "$evidence/nbd-recovery-$number.log" 2>&1
    grep -E '^--- PASS: TestIdleDeviceKernelProbe ' "$evidence/nbd-recovery-$number.log" >/dev/null
  done
fi
mkdir "$evidence/source"
tar --no-same-owner -xf "$candidate/source.tar" -C "$evidence/source"
cd "$evidence/source"
nix develop .#smoke-linux -c bash -seu -- "$candidate" "$evidence" "$remote" "$group" "$test_case" "$role" "$bootstrap" "$host_id" <<'INNER'
candidate=$1; evidence=$2; remote=$3; group=$4; test_case=$5; role=$6; bootstrap=$7; host_id=$8
python3 - "$candidate" "$evidence" "$remote" "$group" "$role" "$bootstrap" "$host_id" <<'PY'
import json,os,shutil,stat,sys,time
from datetime import datetime,timezone
from pathlib import Path
from urllib.parse import urlparse
c,e,remote,group,role,bootstrap,host_id=sys.argv[1:]
def tool(name):
 p=shutil.which(name)
 if p is None:raise SystemExit('missing tool: '+name)
 return p
worker={'WORKER_WORK_DIR':e+'/w','JAILER_CHROOT_DIR':e+'/w/jailer','WORKER_HOST_SECRET_PATH':e+'/w/host-secret.json',
 'WORKER_IMAGES_DIR':c+'/images','WORKER_COMPUTER_DEVICES':'/dev/nbd0 /dev/nbd1',
 'WORKER_COMPUTER_STAGING_MIB':'1024','WORKER_COMPUTER_SAVE_EVERY':'30s',
 'WORKER_LOG_CHUNK_BYTES':'1024','WORKER_LOG_BUFFER_BYTES':'1048576','WORKER_LOG_BUFFER_RECORDS':'1024',
 'WORKER_EXECUTION_SLOTS':'1','VM_VCPUS':'2','VM_MEMORY_MIB':'4096','VM_SCRATCH_DISK_MIB':'4096',
 'WORKER_CAPACITY_VCPUS':'2','WORKER_CAPACITY_MEMORY_MIB':'4096','JAILER_UID':'61001','JAILER_GID':'61001',
 'WORKER_NETWORK_LINK_POOL':'198.18.32.0/24','WORKER_NETWORK_TRANSLATION_POOL':'198.19.32.0/24',
 'WORKER_NETWORK_RESOLVER_IPV4':'127.0.0.1','WORKER_NETWORK_BLOCKED_IPV4_CIDRS':'["0.0.0.0/0"]',
 'FIRECRACKER_PATH':tool('firecracker'),'JAILER_PATH':tool('jailer'),'CPU_TEMPLATE_HELPER_PATH':tool('cpu-template-helper'),
 'IP_PATH':tool('ip'),'NFT_PATH':tool('nft'),'MKFS_EXT4_PATH':os.environ['MKFS_EXT4_PATH'],'MKE2FS_CONFIG_PATH':os.environ['MKE2FS_CONFIG_PATH']}
config={'Worker':c+'/agent-worker','Node':tool('node'),'Driver':c+'/driver.mjs','Model':c+'/model.mjs','Bundle':c+'/bundle',
 'Runtime':c+'/runtime.squashfs','RuntimeDescriptor':c+'/runtime.descriptor.json','Evidence':e+'/proof','WorkerCgroup':group+'/supervisor',
 'RestartPublicationOwner':os.environ.get('HELMR_NATIVE_RESTART_PUBLICATION_OWNER')=='1',
 'CASURI':remote.rstrip('/')+'/cas','PlatformURI':remote.rstrip('/')+'/platform','WorkerEnv':worker,
 'SourceHostID':host_id,'TargetHostID':os.environ.get('HELMR_NATIVE_CROSS_HOST_TARGET_ID','')}
if os.environ.get('HELMR_NATIVE_PERFORMANCE_KIND'):
 assert json.loads(Path(c+'/source.json').read_text()).get('native_performance') is True,'performance requires its instrumented artifact'
 config['Performance']={'Kind':os.environ['HELMR_NATIVE_PERFORMANCE_KIND'],'Degraded':os.environ.get('HELMR_NATIVE_PERFORMANCE_DEGRADED','0')=='1','Background':os.environ.get('HELMR_NATIVE_PERFORMANCE_BACKGROUND','0')=='1'}
 worker['WORKER_COMPUTER_SAVE_EVERY']='5s' if config['Performance']['Background'] else '1h'
with open(e+'/config.json','x') as f:json.dump(config,f,indent=2)
if role=='target':
 assert not os.path.lexists(e+'/w'),'target workroot is not fresh'
 os.mkdir(e+'/proof',0o700)
 parent=Path(bootstrap).parent.stat()
 assert parent.st_uid==0 and stat.S_IMODE(parent.st_mode)==0o700,'bootstrap parent is not root-private'
 waiting=time.monotonic()
 with open(e+'/proof/target-prepared.json.new','x') as f:json.dump({'targetResourceId':host_id,'preparedAt':datetime.now(timezone.utc).isoformat(),'freshWorkroot':True},f)
 os.rename(e+'/proof/target-prepared.json.new',e+'/proof/target-prepared.json')
 while not os.path.lexists(bootstrap):
  if time.monotonic()-waiting>=1200:raise SystemExit('target bootstrap wait exceeded 20 minutes')
  time.sleep(0.1)
 fd=os.open(bootstrap,os.O_RDONLY|os.O_NOFOLLOW)
 with os.fdopen(fd) as f:
  meta=os.fstat(f.fileno())
  assert stat.S_ISREG(meta.st_mode) and meta.st_uid==0 and stat.S_IMODE(meta.st_mode)==0o600 and meta.st_size<=16384,'bootstrap file is not root-private bounded input'
  private=json.load(f)
 assert private['targetHostId']==host_id and private['sourceHostId']!=host_id
 assert private['casUri']==config['CASURI'] and private['platformUri']==config['PlatformURI']
 url=urlparse(private['controlPlaneUrl'])
 assert url.scheme=='http' and url.hostname=='127.0.0.1' and url.port and not url.username and not url.password and url.path=='' and not url.query and not url.fragment
 token=e+'/proof/enrollment-token'
 with open(token,'x') as f:f.write(private['enrollmentToken'])
 os.chmod(token,0o600)
 worker.update({'CONTROL_PLANE_URL':private['controlPlaneUrl'],'CAS_URI':private['casUri'],'PLATFORM_STORE_URI':private['platformUri'],
  'WORKER_ENROLLMENT_TOKEN_FILE':token,'WORKER_POOL_NAME':'native-proof','WORKER_RESOURCE_ID':host_id,'CHECKPOINT_ENCRYPTION_KEY':private['checkpointKey']})
 with open(e+'/proof/target-start.json.new','x') as f:json.dump({'checkpointId':private['checkpointId'],'sourceResourceId':private['sourceHostId'],'targetResourceId':host_id,'freshWorkroot':True,'workerPid':os.getpid(),'bootstrapWaitMs':(time.monotonic()-waiting)*1000,'startedAt':datetime.now(timezone.utc).isoformat()},f)
 os.rename(e+'/proof/target-start.json.new',e+'/proof/target-start.json')
 os.unlink(bootstrap)
 with open(group+'/supervisor/cgroup.procs','w') as f:f.write(str(os.getpid()))
 fd=os.open(e+'/proof/worker.log',os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
 os.dup2(fd,1);os.dup2(fd,2);os.close(fd)
 os.execve(c+'/agent-worker',[c+'/agent-worker'],{**os.environ,**worker})
PY
if [[ "$role" == target ]]; then exit 0; fi
export HELMR_NATIVE_EXECUTION_PROOF=1 HELMR_NATIVE_EXECUTION_CONFIG="$evidence/config.json"
test_timeout=19m
if [[ "$test_case" == TestWorkerAgentPerformance ]]; then test_timeout=42m; fi
if [[ "$test_case" == TestWorkerAgentCheckpointKeyMismatch || "$test_case" == TestWorkerAgentSessionDiscoveryContinuation ]]; then test_timeout=24m; fi
if [[ "$test_case" == TestWorkerAgentLongIdleContinuation ]]; then test_timeout=139m; fi
if [[ "$test_case" == TestWorkerAgentCrossHostContinuation ]]; then test_timeout=57m; fi
if [[ "$test_case" == TestWorkerAgentRetainedNetworkRecovery ]]; then test_timeout=27m; fi
"$candidate/agent-execution.test" -test.run "^${test_case}$" -test.count=1 -test.timeout="$test_timeout" -test.v > "$evidence/execution.log" 2>&1
INNER
if [[ "$role" == source ]]; then
  grep -E "^--- PASS: ${test_case} " "$evidence/execution.log" >/dev/null
fi
