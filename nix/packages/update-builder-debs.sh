#!/usr/bin/env bash
# Resolve the binary toolchain and OpenSSH build dependencies using Debian's
# signed package indexes. Review new versions together with the source pin.
set -euo pipefail
cd "$(dirname "$0")"
base=$(jq -r '.runtimeLibraries | .imageName + "@" + .imageDigest' debian-images.json)
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
docker run --rm --platform linux/amd64 "$base" sh -ec '
  apt-get update >&2
  apt-get -o Acquire::ForceHash=SHA256 --print-uris --yes --no-install-recommends install \
    build-essential ca-certificates git libsqlite3-dev openssh-client pkg-config python3 zlib1g-dev \
    debhelper dh-exec dh-runit dh-sequence-movetousr libaudit-dev libedit-dev libfido2-dev \
    libkrb5-dev libpam0g-dev libselinux1-dev libssl-dev libwrap0-dev libwtmpdb-dev fakeroot
' > "$tmp"
python3 - "$tmp" <<'PY'
from concurrent.futures import ThreadPoolExecutor
import hashlib
import re
import urllib.request
import shlex
import sys
from pathlib import Path

archives = {}
for line in Path(sys.argv[1]).read_text().splitlines():
    if not line.startswith("'"):
        continue
    url, _, _, digest = shlex.split(line)
    match = re.fullmatch(r"https?://deb\.debian\.org/((?:debian|debian-security)/pool/\S+\.deb)", url)
    if not match or not re.fullmatch("SHA256:[0-9a-f]{64}", digest):
        raise ValueError(f"unexpected APT archive: {url}")
    archives[match[1]] = digest.removeprefix("SHA256:")
if not archives:
    raise ValueError("APT returned no archives")
def locked(path):
    with urllib.request.urlopen("https://deb.debian.org/" + path, timeout=90) as response:
        data = response.read()
    if hashlib.sha256(data).hexdigest() != archives[path]:
        raise ValueError(f"archive differs from signed APT index: {path}")
    # Debian Snapshot addresses the file by SHA-1. Nix verifies SHA-256.
    return f"{archives[path]} {hashlib.sha1(data).hexdigest()} {path}\n"

with ThreadPoolExecutor(max_workers=4) as workers:
    contents = "".join(workers.map(locked, sorted(archives)))
Path("builder-debs.lock").write_text(contents)
PY
