#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
attempt=$(mktemp -d)
trap 'rm -rf "$attempt"' EXIT
mkdir "$attempt/bin"
# Exercise the actual functions without running PID 1 mounts on the test host.
# Only kernel input and resolver output paths are redirected into the fixture.
sed '/^mount_base$/,$d' "$repo_root/images/guest/init.sh" |
  sed -e "s|/proc/cmdline|$attempt/cmdline|g" -e "s|/run/resolv.conf|$attempt/resolv.conf|g" > "$attempt/init.sh"
cat > "$attempt/bin/ip" <<'IP'
#!/bin/sh
printf '%s\n' "$*" >> "$IP_LOG"
[ "$*" != "${FAIL_IP:-}" ] || exit 42
case "$*" in
  'link show dev eth0'|'link set eth0 up'|'addr flush dev eth0'|\
  'addr add 192.168.127.2/30 dev eth0'|'route replace default via 192.168.127.1 dev eth0') ;;
  'route show default') printf 'default via 192.168.127.1 dev eth0\n' ;;
  *) exit 43 ;;
esac
IP
chmod +x "$attempt/bin/ip"
export PATH="$attempt/bin:$PATH" IP_LOG="$attempt/ip.log"
static='helmr.ip=192.168.127.2::192.168.127.1:255.255.255.252::eth0:off:10.0.0.2::'

run_case() {
  local name=$1 expected=$2 cmdline=$3
  printf '%s\n' "$cmdline" > "$attempt/cmdline"
  : > "$IP_LOG"
  local rc=0
  # Calling in a condition intentionally disables errexit inside the functions.
  "${GUEST_INIT_TEST_SHELL:-sh}" -c '. "$1"; if configure_network; then exit 0; else exit 1; fi' sh "$attempt/init.sh" > "$attempt/output" 2>&1 || rc=$?
  if [[ "$rc" != "$expected" ]]; then
    cat "$attempt/output" >&2
    printf 'FAIL %s: exit %s, expected %s\n' "$name" "$rc" "$expected" >&2
    exit 1
  fi
  printf 'ok - %s\n' "$name"
}

run_case static 0 "console=ttyS0 $static"
printf 'nameserver 10.0.0.2\n' > "$attempt/expected-dns"
cmp "$attempt/expected-dns" "$attempt/resolv.conf"
cat > "$attempt/expected-ip" <<'IP'
link show dev eth0
link set eth0 up
addr flush dev eth0
addr add 192.168.127.2/30 dev eth0
route replace default via 192.168.127.1 dev eth0
route show default
IP
cmp "$attempt/expected-ip" "$IP_LOG"

for invalid in \
  '' 'helmr.ip=' "helmr.ip $static" "helmr.network $static" "$static $static" "helmr.ip= $static" \
  "${static/helmr.ip=/ip=}" "${static/helmr.ip=/nfsaddrs=}" \
  "${static/192.168.127.2/}" "${static/192.168.127.2/999.1.1.1}" \
  "${static/192.168.127.2/192.168.127.2.}" "${static/192.168.127.2/0192.168.127.2}" \
  "${static/192.168.127.1/}" "${static/255.255.255.252/255.0.255.0}" \
  "${static/255.255.255.252/}" "${static/eth0/eth1}" "${static/eth0/}" \
  "${static/off/dhcp}" "${static/10.0.0.2/}" "${static/10.0.0.2/0.0.0.0}" \
  "${static/10.0.0.2/1.2.3}" "${static}extra" \
  "$static helmr.network=none" 'helmr.network=none helmr.network=none' 'helmr.network=auto'; do
  run_case "reject $invalid" 1 "$invalid"
  [[ ! -s "$IP_LOG" ]] || { echo 'invalid input reached ip' >&2; exit 1; }
done

while IFS= read -r command; do
  export FAIL_IP=$command
  run_case "command failure: $command" 1 "$static"
  [[ "$(tail -n 1 "$IP_LOG")" == "$command" ]] || { echo 'continued after command failure' >&2; exit 1; }
done < "$attempt/expected-ip"
unset FAIL_IP
rm "$attempt/resolv.conf"
mkdir "$attempt/resolv.conf"
run_case 'resolver write failure' 1 "$static"
run_case 'network-none resolver write failure' 1 'helmr.network=none'
rmdir "$attempt/resolv.conf"
run_case network-none 0 'helmr.network=none'
[[ ! -s "$IP_LOG" && -f "$attempt/resolv.conf" && ! -s "$attempt/resolv.conf" ]]
