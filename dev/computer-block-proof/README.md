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

## Prepared paired Firecracker fixture (not yet KVM-qualified)

`vm-proof.py` and `cmd/guest-oracle` prepare the next experiment: an actual
Firecracker 1.17.0 full snapshot containing RAM/device state paired with a pinned
Computer generation and an independent scratch image. This fixture has local
syntax/contract-test and static Linux amd64 build evidence only. It has **not** run
on KVM. A successful build or the earlier kernel NBD proof is not VM evidence.

The fixture reuses `images/guest/out/{vmlinuz,initramfs,rootfs.squashfs}` after
checking every digest/size against `runtime-artifacts.json` (amd64). Those existing
assets currently identify kernel `9a3bc8b89f703de9e84eb3c52e06f8e97998e88b9350e20bdb7f781b5e62f06e`,
initramfs `958be56d0de5a0bcc66cb1ea23f9ae60eb46e1e415a7919641d4f0d54e3f9b55`,
and rootfs `77ce101d468b9f5d9216c4d64a0ca056ae335c64651f9806431adc83880d526c`.
`images/boot-artifacts.mk` and `images/guest/Makefile` build these via
`nix develop .#images -c make -C images/guest all` when regeneration is needed;
regenerated artifacts need fresh evidence. The pinned Alpine initramfs honors
`init=/bin/sh`. A bounded serial bootstrap mounts scratch and executes the static
oracle seeded on it; writable mountpoints live under `/run` tmpfs. No production
init or guestd modification, network or vsock is needed.

The host formats only newly created 16 MiB regular seed files, then copies the
Computer seed to a device it atomically claimed through the unchanged
`kernel-proof.py` attachment helper. Both ext4 seeds contain a readable zero marker
file. The guest replaces both markers with fresh bytes, fsyncs them, retains a
random nonce only in live RAM, and announces READY. The host acknowledges Pause,
checks Sync device engines, fsyncs retained Computer/scratch descriptors, creates
a Full snapshot with file syncing enabled, checks host flush errors again, and
copies scratch plus the pinned Computer root. Pause alone is not the full barrier;
[Firecracker 1.17.0 snapshot creation drains and syncs block backings](https://github.com/firecracker-microvm/firecracker/blob/v1.17.0/docs/snapshotting/snapshot-support.md).

The source VMM is terminated and reaped before the backend is stopped/disconnected.
Each restore uses pristine RAM snapshot bytes, an independent writable scratch
copy and backend branch, and a distinct newly claimed NBD device. The original
RAM nonce and a fresh host challenge must appear in a post-resume response.
Aligned guest O_DIRECT reads must succeed on **both** files, without buffered
fallback. Three cases must produce exactly: wrong Computer `(false, true)`, wrong
scratch `(true, false)`, and paired `(true, true)`. Missing files, IO errors, crashes
or timeouts do not satisfy a negative case. These are oracle checks of pairing,
not automatic production restore validation.

Prepare locally:

```sh
nix develop .#default -c env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -o /tmp/helmr-vm-guest-oracle-amd64 ./dev/computer-block-proof/cmd/guest-oracle
nix develop .#default -c env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -o /tmp/helmr-vm-block-proof-amd64 ./dev/computer-block-proof/cmd/block-proof
nix develop .#default -c python3 -B dev/computer-block-proof/vm-proof-test.py
```

Execution remains gated on an explicitly authorized disposable **x86_64 Linux**
host with usable `/dev/kvm`, four unused accessible `/dev/nbd0`–`/dev/nbd15` devices,
permission for NBD ioctls, `mkfs.ext4`, Python 3.11+, and the pinned Firecracker
1.17.0 binary. The repository's `.#smoke-linux` shell supplies the pinned
Firecracker and image tooling on x86_64 Linux. For that host only:

```sh
# Paths below must refer to the verified assets/binaries on that disposable host.
proof_arena=$(mktemp -d /tmp/helmr-vm-proof.XXXXXX)
# Run GNU timeout inside the pinned shell. Its normal process-group supervision
# includes this fixture's inherited Firecracker/backend/NBD helper descendants.
if HELMR_DISPOSABLE_VM_PROOF=1 nix develop .#smoke-linux -c \
  timeout --signal=TERM --kill-after=15s 260s \
  python3 -B dev/computer-block-proof/vm-proof.py \
  --arena "$proof_arena" \
  --firecracker /absolute/path/to/firecracker \
  --backend /tmp/helmr-vm-block-proof-amd64 \
  --oracle /tmp/helmr-vm-guest-oracle-amd64 \
  --assets "$PWD/images/guest/out"; then
  proof_status=0
else
  proof_status=$?
fi
# Mandatory read-only postflight, including after timeout/KILL. Inspect known
# claims in $proof_arena/owned-devices.json if the arena was retained. An abrupt
# kill may precede journaling; inspect every exposed candidate as well.
proof_postflight=0
for proof_device in /sys/block/nbd*/pid; do
  if [ -e "$proof_device" ]; then
    printf 'ACTIVE: %s\n' "$proof_device"
    cat "$proof_device"
    proof_postflight=1
  fi
done
printf 'proof exit=%s; failed-run arena=%s\n' "$proof_status" "$proof_arena"
# Any nonzero status or unexpected active device leaves acceptance incomplete.
# Do not clear devices by name or delete a retained arena during inspection.
[ "$proof_status" -ne 0 ] || proof_status=$proof_postflight
(exit "$proof_status")
```

The fixture's 240-second signal alarm is a **soft** limit, below the backend's
five-minute connection deadline. Use the external TERM/KILL supervisor above;
a blocked kernel ioctl or process in uninterruptible sleep can outlive signals,
so even that deadline is not a guarantee of cleanup. Inspect the supervisor exit,
owned-device journal and candidate device PIDs before declaring completion. Never
automatically disconnect or clear a device by its pathname after a forced kill.

The fixture prints its private arena before resource allocation and records known
claimed-device identities. It stops VMMs before clearing owned NBD attachments,
then verifies owned devices are inactive. Only that successful path removes the
arena. Execution, resource-cleanup and postflight failures retain the arena and
traceback for inspection. A failure or interruption during final arena removal
may leave only some files, after owned resources are already gone. A forced kill
before that removal likewise leaves the arena behind. Retaining snapshots and
local state on failed execution is intentional. A failure does not authorize broader infrastructure
changes. No KVM is available in the current local environment;
there has been no AWS operation. This direct API experiment deliberately bypasses
the production connector, whose backing validator requires regular files. It
qualifies neither that connector nor remote publication, host reboot/power loss,
application/database quiescence, runtime SDK behavior or cross-machine CPU
compatibility. All branches and snapshots remain disposable local fixture data.

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
