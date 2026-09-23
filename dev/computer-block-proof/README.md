# Computer block proof (development only)

This isolated Go experiment models a finite, 4 KiB-granularity disk backed by
immutable packed objects. It is not connected to Helmr runtime, SDK, a VM, a
kernel device, AWS or S3. Its only dependency is Go's standard library.

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

## Transport boundary

`ServeTransmission` implements NBD simple request/reply transmission framing on a
caller-owned `io.ReadWriter`. Tests use `net.Pipe` plus byte-fragmented streams;
they do not open a socket listener. READ, WRITE, aligned TRIM and DISC are modeled.
The adapter caps requests at 1 MiB before allocating. Invalid magic or oversized
requests end the stream; bounded rejected WRITE bodies are consumed to preserve
framing. Reads complete and validate before a successful reply is emitted.
Unsupported commands, flags (including FUA) and FLUSH return EOPNOTSUPP. No FLUSH
success is reported: this experiment has no guest durability barrier.

There is no handshake/option negotiation, feature advertisement, structured reply,
full NBD client compatibility, kernel attachment, root filesystem mounting or
microVM execution. The caller owns connection closing, deadlines and cancellation.
This is a protocol-adapter test, not proof of a guest/device capture boundary.

## Deliberate limits

- The object store is a local-file stand-in; it does not model S3 consistency,
  multipart upload, throttling, credentials or network behavior.
- File contents are synced during staging, but directory entries and the head are
  not durably committed. No process-restart recovery of a latest head, power-loss
  guarantee, distributed lease or database transaction is established. A known
  pin can be reopened while its local objects remain present.
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

The next acceptance boundary is a separately authorized real block-device and
VM integration with explicit guest flush/capture ordering and durable recovery.
