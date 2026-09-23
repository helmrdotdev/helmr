# Computer preservation format experiment

This is a local experiment, not a production storage implementation or benchmark.
It creates a 128 MiB deterministic disk file, takes stable views, changes eight
4 KiB ranges, and compares full-manifest chunk sizes. It checks original snapshot
integrity after source mutation, reconstruction of both versions, and rejection of
an incomplete candidate without replacing the previous in-memory published head.

The object dictionary is plaintext and process-local. It performs no remote upload,
encryption, database publication, ownership fencing, guest pause, or concurrent
workload measurement. `new_object_bytes` is logical new content, not wire bytes or
billed storage. Every manifest pass reads the full file. Do not use these numbers
as guest latency, storage durability, or production performance claims.

The default mode uses a full copy while the writer is stopped. For a Linux
filesystem supporting reflink, set `PROOF_REQUIRE_REFLINK=1`; unsupported reflink
fails rather than falling back to full copying. Python 3.11 or later is required.

```sh
python3 dev/computer-preservation-proof/chunks.py
TMPDIR=/path/to/isolated/reflink-filesystem PROOF_REQUIRE_REFLINK=1 \
  python3 dev/computer-preservation-proof/chunks.py
```

Qualification used a disposable Docker Desktop Linux container, one CPU and
768 MiB RAM, Linux 7.0.12-linuxkit arm64, Python from the local image
`node@sha256:5a593d74b632d1c6f816457477b6819760e13624455d587eef0fa418c8d0777b`.
The container's default overlayfs rejected reflink. A fresh 1 GiB loopback XFS
fixture, formatted with xfsprogs 6.1.0-1 and reflink enabled, supported it. The
fixture was unmounted and the container removed. No host or deployed disk was
formatted. XFS tooling was installed inside that temporary container; the package
installation was not a pinned release build.

A single small fixture run does not select a chunk size or a Worker filesystem.
Large/fragmented disks, COW allocation pressure, sparse holes, real guest barriers,
SQLite/WAL recovery, encryption, remote publication, stale-writer rejection,
user-visible latency and multi-Computer contention remain unqualified.
