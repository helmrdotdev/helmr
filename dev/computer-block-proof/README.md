# Computer block proof (development only)

This isolated Go experiment models a finite, 4 KiB-granularity disk backed by
immutable packed objects. It is not connected to Helmr runtime, SDK, a VM,
AWS or S3. The core uses only Go's standard library; the separate kernel
fixture below uses Python and disposable Linux container access.

Run from the repository root with its pinned toolchain:

```sh
nix develop .#default -c go test ./dev/computer-block-proof/...
nix develop .#default -c go test -race ./dev/computer-block-proof/...
```

## What the executable tests demonstrate

`internal/blockproof` owns the development-only model. `NewDisk` creates zeroed
finite geometry. `WriteAt` tracks complete dirty blocks and handles unaligned
writes by reading the prior block. Bounds and capacity are checked for the whole
request, and prior-block reads finish before any mutation. `Trim` deliberately
means block-aligned zeroing. A capacity error is explicit ENOSPC in the protocol
adapter, not transparent guest backpressure.

`Capture` freezes the dirty map, allowing later writes in another map during
object staging. Both maps count against the configured dirty block budget,
including duplicate block versions. Each capture packs nonzero dirty blocks into
one content-addressed immutable segment, stores their offsets and SHA-256 hashes
in a complete logical manifest, and returns the manifest's content hash. An absent
entry in that complete index means zero, including discarded inherited blocks.
Failure keeps the frozen bytes available for retry, preserving any newer writes.
Only one capture per disk can run at a time. Captured objects are immutable through
the store API; test corruption deliberately bypasses that API.

`Restore` opens exactly the pinned manifest, with no latest lookup and no segment
fetch. Subsequent reads fetch only the needed 4 KiB ranges and verify their hashes;
there is no clean-block cache. A restored disk can branch and capture a new pin
without modifying its source. `Head.Publish` checks the manifest and every mapped
block before comparing the owner epoch and expected predecessor under one mutex.
A failed upload, missing/corrupt object, stale owner or stale predecessor cannot
advance the head. Claiming a new epoch invalidates an older publisher.

Tests use deterministic byte-array oracles, overlapping partial writes, immutable
historical pins, branches, zeroing, upload/manifest failures, missing/corrupt blocks,
manifest corruption, concurrent capture/write, capacity exhaustion, atomic failure
of read-modify-write, and framed protocol requests.

## Recoverable local FLUSH

`CreatePersistent` explicitly initializes a missing root; `OpenPersistent` requires
an existing root and validates all its referenced data before returning. An
advisory `flock` held for the lifetime of `Persistent` refuses another owner.
`Close` releases ownership without flushing unacknowledged writes; process death
also releases the lock. The local directory must be private and on an intact local
filesystem. Other programs that bypass the lock are outside this model.

`Flush` serializes capture and local root publication. Object content is synced
before immutable linking; the object directory is then synced. After all mappings
have been validated, the new root file is synced, atomically renamed over `root`,
and its directory is synced before success. Successful FLUSH covers writes that
completed before that flush began. Concurrent later writes may remain dirty. An
older flush cannot overwrite a newer root. Failed publication retains the captured
base in memory so a retry can commit it even when no dirty blocks remain.

Local `root` is independent of the process-local remote `Head` model. A failure
before root rename leaves the prior root; an error after rename can leave the new
coherent root. A failed FLUSH does not promise rollback. Missing/corrupt referenced
objects fail both open and FLUSH. `OpenPersistent` eagerly verifies all mapped
blocks; the standalone pinned `Restore` model remains lazy.

Subprocess tests kill the owner after acknowledged FLUSH and at deterministic
segment, manifest, root-file, rename and directory-commit boundaries, then reopen
and compare the whole disk. Tests also cover lock release, concurrent FLUSH,
failed-FLUSH retry, missing/corrupt dependencies and socket FLUSH error replies.
These demonstrate process-crash recovery on an intact filesystem; they do not
simulate power loss, a host reboot, controller caches or filesystem failure.

## Transport and private Unix command

`Serve` provides bounded fixed-newstyle NBD negotiation for one unnamed export
through EXPORT_NAME. Unsupported options, including GO, receive ERR_UNSUP; clients
must use/fall back to EXPORT_NAME. It advertises HAS_FLAGS and SEND_TRIM, and
SEND_FLUSH only for `Persistent`. It never advertises FUA. Option payloads are
limited to 4 KiB and negotiation to 16 options.

`ServeTransmission` handles simple request/reply framing on a caller-owned stream.
READ, WRITE, aligned TRIM and DISC are modeled. Requests are capped at 1 MiB before
allocation. Invalid magic or oversized requests end the stream; bounded rejected
WRITE bodies are consumed to preserve framing. Reads validate before a successful
reply. Unsupported commands/flags, including FUA, return EOPNOTSUPP. FLUSH is
supported only by the persistent local implementation; volatile `Disk` rejects it.
The caller owns connection closing, deadlines and cancellation.

Run the private Unix socket command (no kernel attachment):

```sh
# Choose a private directory and a socket path that does not already exist.
nix develop .#default -c go run ./dev/computer-block-proof/cmd/block-proof \
  -create -dir /tmp/my-block-proof-state -socket /tmp/my-block-proof.sock \
  -size 67108864 -limit 128
# To reopen, omit -create. Size is used only for initial creation.
```

The command uses a restrictive umask before binding and accepts one client at a
time. SIGINT/SIGTERM close the listener and active connection. Connections have a
five-minute total deadline. It refuses an existing socket path rather than
unlinking it; after a crash, inspect and remove only your own stale socket, or use
a new path. A socket client can mutate this development disk, so keep its parent
directory private. No TCP listener is provided.

Tests exercise framing through `net.Pipe` and fragmented byte streams, negotiation,
feature flags and persistent FLUSH. There is no TLS, structured reply, multi-client
write coordination, filesystem mounting or microVM integration. This package does
not attach a kernel device; any separately run device test has its own evidence.

## Recorded kernel NBD proof

On 2026-09-23, `kernel-proof.py` passed against Linux `7.0.12-linuxkit` arm64.
The fixture wrote 4 KiB of random bytes through `/dev/nbd15`, completed `fsync`,
killed the server, reopened its local persistent state, attached `/dev/nbd14`,
and verified the bytes using an aligned O_DIRECT read. It exited zero, reported
owned-device cleanup complete, and a separate postflight found no active PID for
either device. Only the pre-existing build container remained. This establishes
the exercised raw Linux block write/FLUSH/server-restart/read path, without
formatting or mounting any filesystem.

The reviewed fixture is preserved byte-for-byte with SHA-256
`b62ddf6b3724a1e00c13081ccbdb0e65e83bcfe7747fbb7a26774485d7d4a65d`.
The tested static Linux arm64 executable had SHA-256
`c3d299fed40bb3caa57b88782bec7d5dc9503fcecd9e9bc51ffc5a17370184c4`.
A rebuilt executable may have another hash; its run needs its own evidence.

Reproduction requires an explicitly authorized disposable Linux environment with
NBD support and unused `/dev/nbd14` and `/dev/nbd15`. Container device access affects
the shared host kernel: inspect ownership before running and verify inactive PIDs
afterwards. The fixture skips busy devices, claims a device with exclusive open
and SET_SOCK, and only disconnects/clears descriptors it claimed. Do not substitute
in-use devices or run this as an unattended generic CI check.

From the repository root, build with the pinned toolchain, then run the fixture:

```sh
nix develop .#default -c env CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -o /tmp/helmr-computer-block-proof-linux-arm64 \
  ./dev/computer-block-proof/cmd/block-proof

docker run --rm --pull=never --name helmr-nbd-kernel-proof \
  --network none --cap-add SYS_ADMIN \
  --device /dev/nbd14 --device /dev/nbd15 \
  --memory 384m --cpus 1 --stop-timeout 30 \
  -e HELMR_DISPOSABLE_NBD_PROOF=1 \
  -v "$PWD/dev/computer-block-proof/kernel-proof.py:/proof.py:ro" \
  -v /tmp/helmr-computer-block-proof-linux-arm64:/block-proof:ro \
  node@sha256:5a593d74b632d1c6f816457477b6819760e13624455d587eef0fa418c8d0777b \
  python3 /proof.py /block-proof
```

The pinned image must already be present; this command does not pull it. Temporary
state stays inside the removed container. This test does not prove host power-loss
or reboot recovery, filesystem/database consistency, guest/VM behavior, remote
object-store behavior or production readiness.

## Deliberate limits

- The object store is a local-file stand-in; it does not model S3 consistency,
  multipart upload, throttling, credentials or network behavior.
- Process-crash recovery applies only to the local persistent root on intact
  storage. The remote Head is still process-local. No power-loss guarantee,
  distributed lease or database transaction is established.
- The budget bounds retained dirty payload blocks, not total process memory or
  object storage. Completed captures release dirty memory after both objects and
  manifest are staged, even before publication; their stored objects remain.
  Packing and whole-request staging create additional temporary copies. Complete
  index cloning/serialization is proportional to all mapped blocks, so this is
  not a scalable metadata design. Old objects accumulate without GC.
- No authentication, encryption, corruption repair, compression, sparse object
  packing policy, multi-disk atomicity, application quiescence or database
  consistency is claimed. SHA-256 detects corruption, not authenticity.
- Publication assumes immutable objects remain available after validation. The
  local mutex is process-local and does not fence an actual device or VM writer.
  Integrity validation scans all referenced blocks and is intentionally expensive.

Guest filesystem and VM acceptance still require separate evidence;
local FLUSH success does not establish application/database quiescence or remote
publication. This development format has no compatibility or migration promise.
