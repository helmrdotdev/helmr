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

## Filesystem and transfer comparison

`compare-local.sh` compares XFS reflink plus 1 MiB content-addressed chunks with
Btrfs read-only subvolume snapshots plus native incremental send/receive, including
a Btrfs NOCOW variant. `btrfs-chunks` runs the identical chunk algorithm on
a Btrfs snapshot to separate filesystem from archive-format effects. It creates only owned loopback image files. Run it **only in
a disposable privileged Linux container**, never directly on a Worker or host:

```sh
docker run --rm --privileged --memory 1536m --cpus 2 \
  -e HELMR_DISPOSABLE_STORAGE_FIXTURE=1 \
  -v "$PWD/dev/computer-preservation-proof:/proof:ro" \
  node@sha256:5a593d74b632d1c6f816457477b6819760e13624455d587eef0fa418c8d0777b \
  sh -ec 'apt-get update -qq; apt-get install -y -qq xfsprogs btrfs-progs; sh /proof/compare-local.sh'
```

The 256 MiB deterministic image receives two generations of dispersed 128 KiB
changes and a 32 MiB random rewrite. Each generation is retained and checked for
immutability; the last is reconstructed on a separately formatted receiver and
verified by digest. Incremental receive without its parent must fail. The final
rewrite leaves dirty pages for capture's timed fsync. Rotating
`HELMR_FIXTURE_ORDER` between `xfs btrfs btrfs-nocow`, `btrfs-nocow xfs btrfs`, and
`btrfs btrfs-nocow xfs` supports rotated-order trials. Include
`btrfs-chunks` in each order for the four-arm comparison. Tool versions and
JSON results are printed; archives disappear with the disposable container.

This compares complete candidate paths, not intrinsic filesystem speed: the XFS
implementation hashes every byte in Python; Btrfs serializes native extent changes.
XFS uses a direct clone ioctl; Btrfs uses subprocesses. Archive data is flushed to
local files, without durable directory publication or remote storage. Receiver and
source have separate filesystems but share the same underlying Docker storage.
`saved_bytes` counts chunk payloads (excluding their manifest) or full stream
bytes. `missing_parent_check_ms` is excluded from successful `restore_ms`.

The same-filesystem write/fsync probe is outside the captured subvolume, is not a
guest workload, and may collect only one sample during short sends. Do not interpret
its p95 as a service SLO. View and archive probes are reported separately;
percentiles are omitted for fewer than 20 samples. The free-space delta is a coarse filesystem counter, not
comparable exclusive extent usage or billing. NOCOW properties after receive are
reported rather than assumed. Sparse files, long chains, ENOSPC/reclamation,
concurrent Computers, crash/power loss and actual Worker kernels remain untested.
