#!/usr/bin/env bash
# Install the same verified Worker payload on an image builder or disposable host.
# This does not enroll/start a Worker, partition disks, or contact a cloud provider.
set -euo pipefail

if [ "$#" != 8 ]; then
  echo 'usage: install-worker-host.sh verify|install HOST_TAR HOST_SHA256 HOST_MANIFEST_SHA256 RUNTIME_TAR RUNTIME_SHA256 RUNTIME_MANIFEST_SHA256 PREPARE_ROOT_SCRIPT' >&2
  exit 2
fi
action=$1
shift
case "$action" in verify|install) ;; *) echo "expected verify or install" >&2; exit 2 ;; esac
host_bundle=$1
host_digest=${2#sha256:}
host_manifest_digest=${3#sha256:}
runtime_bundle=$4
runtime_digest=${5#sha256:}
runtime_manifest_digest=${6#sha256:}
prepare_root=$7
for digest in "$host_digest" "$host_manifest_digest" "$runtime_digest" "$runtime_manifest_digest"; do
  [[ "$digest" =~ ^[0-9a-f]{64}$ ]] || { echo 'invalid artifact digest' >&2; exit 1; }
done
if [ "$action" = install ]; then
  [ "$(uname -s):$(uname -m)" = Linux:x86_64 ] || { echo 'Linux x86_64 is required' >&2; exit 1; }
  [ "$(id -u)" = 0 ] || { echo 'root is required' >&2; exit 1; }
  for command in install systemctl groupadd useradd sysctl getent; do command -v "$command" >/dev/null; done
  if [ "$(systemctl show helmr-worker.service -p ActiveState --value)" != inactive ]; then
    echo 'stop and quiesce the Worker before replacing its installed artifacts' >&2
    exit 1
  fi
fi
[ -f "$prepare_root" ] && [ ! -L "$prepare_root" ] || { echo 'prepare-root script must be a regular file' >&2; exit 1; }
# Installation is offline; provisioning owns OS packages and artifact transport.
for command in jq sha256sum tar; do
  command -v "$command" >/dev/null
done
work=$(mktemp -d "${TMPDIR:-/tmp}/helmr-worker-install.XXXXXX")
trap 'rm -rf "$work"' EXIT
chmod 0700 "$work"
mkdir "$work/host" "$work/runtime"
# Use verified private copies for extraction; do not reopen mutable input paths.
cp "$host_bundle" "$work/worker-host-artifacts.tar"
cp "$runtime_bundle" "$work/runtime-artifacts.tar"
printf '%s  %s\n' "$host_digest" "$work/worker-host-artifacts.tar" | sha256sum -c -
printf '%s  %s\n' "$runtime_digest" "$work/runtime-artifacts.tar" | sha256sum -c -
expected_host_artifacts="$(printf '%s\n' cpu-template-helper firecracker jailer mkfs.ext4 worker mke2fs.conf worker-host-artifacts.json)"
actual_host_artifacts="$(tar -tf "$work/worker-host-artifacts.tar")"
test "$actual_host_artifacts" = "$expected_host_artifacts"
test -z "$(tar -tvf "$work/worker-host-artifacts.tar" | sed -n '/^-/!p')"
tar --no-same-owner --same-permissions -C "$work/host" -xf "$work/worker-host-artifacts.tar"

printf '%s  %s\n' "$host_manifest_digest" "$work/host/worker-host-artifacts.json" | sha256sum -c -
jq -e '
  (keys | sort) == ["arch", "files", "schema"] and
  .schema == "helmr.worker-host-artifacts.v0" and
  .arch == "amd64" and
  [.files[].path] == ["cpu-template-helper", "firecracker", "jailer", "mkfs.ext4", "worker", "mke2fs.conf"] and
  all(.files[];
    ((.path == "mke2fs.conf" and .mode == "0444") or (.path != "mke2fs.conf" and .mode == "0755")) and
    (.size_bytes | type == "number" and . > 0) and
    (.digest | test("^sha256:[0-9a-f]{64}$")))
' "$work/host/worker-host-artifacts.json" >/dev/null

while IFS=$'\t' read -r path digest size_bytes mode; do
  file="$work/host/$path"
  test -f "$file" && test ! -L "$file"
  test "$(sha256sum "$file" | awk '{print $1}')" = "${digest#sha256:}"
  test "$(stat -c %s "$file")" = "$size_bytes"
  test "$(stat -c %a "$file")" = "${mode#0}"
done < <(jq -r '.files[] | [.path, .digest, .size_bytes, .mode] | @tsv' "$work/host/worker-host-artifacts.json")

expected_runtime_artifacts="$(printf '%s\n' initramfs rootfs.squashfs runtime-artifacts.json vmlinuz)"
actual_runtime_artifacts="$(tar -tf "$work/runtime-artifacts.tar")"
test "$actual_runtime_artifacts" = "$expected_runtime_artifacts"
test -z "$(tar -tvf "$work/runtime-artifacts.tar" | sed -n '/^-/!p')"
tar --no-same-owner --same-permissions -C "$work/runtime" -xf "$work/runtime-artifacts.tar"
printf '%s  %s\n' "$runtime_manifest_digest" "$work/runtime/runtime-artifacts.json" | sha256sum -c -

jq -e '.schema == "helmr.runtime-artifacts.v0" and .arch == "amd64" and .vm_runtime_contract == "helmr.vm-runtime.v0"' "$work/runtime/runtime-artifacts.json" >/dev/null
for entry in kernel:vmlinuz initramfs:initramfs rootfs:rootfs.squashfs; do
  key=${entry%%:*}
  name=${entry#*:}
  digest=$(jq -er --arg key "$key" '.[$key].digest' "$work/runtime/runtime-artifacts.json")
  size=$(jq -er --arg key "$key" '.[$key].size_bytes' "$work/runtime/runtime-artifacts.json")
  [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]
  printf '%s  %s\n' "${digest#sha256:}" "$work/runtime/$name" | sha256sum -c -
  test "$(stat -c %s "$work/runtime/$name")" = "$size"
done
if [ "$action" = verify ]; then
  echo 'verified Worker host and runtime artifacts'
  exit 0
fi
install -d -m 0755 /usr/local/sbin
install -m 0755 "$prepare_root" /usr/local/sbin/helmr-prepare-root
install -d -m 0755 /usr/local/bin /usr/share/helmr /var/lib/helmr/images/guest/out
for name in cpu-template-helper firecracker jailer worker; do
  install -m 0755 "$work/host/$name" "/usr/local/bin/$name"
done
test -x /usr/local/bin/cpu-template-helper
install -d -m 0755 /usr/local/libexec/helmr
install -m 0755 "$work/host/mkfs.ext4" /usr/local/libexec/helmr/mkfs.ext4
install -m 0444 "$work/host/mke2fs.conf" /usr/share/helmr/mke2fs.conf
install -m 0644 "$work/host/worker-host-artifacts.json" /usr/share/helmr/worker-host-artifacts.json
for name in initramfs rootfs.squashfs runtime-artifacts.json vmlinuz; do
  install -m 0444 "$work/runtime/$name" "/var/lib/helmr/images/guest/out/$name"
done
getent group helmr-vmm >/dev/null || groupadd --system --gid 1001 helmr-vmm
test "$(getent group helmr-vmm | cut -d: -f3)" = 1001
id helmr-vmm >/dev/null 2>&1 || useradd --system --uid 1001 --gid 1001 --groups kvm --no-create-home --shell /usr/sbin/nologin helmr-vmm
test "$(id -u helmr-vmm):$(id -g helmr-vmm)" = 1001:1001
getent group helmr-verifier >/dev/null || groupadd --system helmr-verifier
id helmr-verifier >/dev/null 2>&1 || useradd --system --gid helmr-verifier --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin helmr-verifier
cat >/etc/udev/rules.d/99-helmr-kvm.rules <<'EOF'
KERNEL=="kvm", GROUP="helmr-vmm", MODE="0660"
EOF
if [ -e /dev/kvm ]; then
  chgrp helmr-vmm /dev/kvm
  chmod 0660 /dev/kvm
fi

install -d -m 0755 /run/helmr /etc/helmr /var/lib/helmr /var/lib/helmr/jailer
cat >/etc/systemd/system/helmr-worker.service <<'EOF'
[Unit]
Description=Helmr worker
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/helmr/worker.env
ExecStart=/usr/local/bin/worker
Restart=on-failure
RestartSec=5
Delegate=yes
DelegateSubgroup=supervisor
KillMode=mixed
TasksMax=infinity

[Install]
WantedBy=multi-user.target
EOF

cat >/etc/sysctl.d/99-helmr-firecracker.conf <<'EOF'
net.ipv4.ip_forward = 1
user.max_user_namespaces = 16384
EOF

sysctl -p /etc/sysctl.d/99-helmr-firecracker.conf
systemctl daemon-reload
