# Native Computer continuation fixture

## Integrated execution qualification

The separate `TestWorkerAgentNativeExecution` fixture uses a compiled application,
the public SDK, authenticated CP HTTP with real PostgreSQL, production Secret/key
and S3 services, running allocation/preparation/Session/Computer/Command owners,
and the unchanged worker executable. Bootstrap inserts tenant prerequisites only;
worker enrollment, deployment admission, preparation, leases, process attachment,
Turn finalization and save publication use their ordinary owners.

Build a candidate from a clean commit on x86 Linux with local Docker:

```sh
nix develop .#images --command tests/build/native-execution-package.sh /absolute/new/candidate
```

When the x86 Linux build shell itself runs inside Docker Desktop with the local
Docker socket mounted, set `HELMR_NATIVE_REGISTRY_HOST=host.docker.internal` so
it reaches the daemon's published loopback registry. A direct Linux host uses
the default `127.0.0.1`. Neither mode publishes a registry beyond host loopback.

The builder, Runtime and local SDK packages are built from that checkout. The
example dependencies are installed only after their local SDK/protocol packages
exist. A temporary local registry
supplies the builder by digest to the current `helmr build` command. The authored
fixture helpers are bundled into a source module before ordinary compilation;
the compiler produces the definition index and the normal builder produces the
Program and canonical Computer seed. The candidate records source, builder and
Runtime identities, includes the current worker, guest boot files, SDK driver and
test binary, and checksums every file. This build does not run a VM or contact the
remote qualification store.

After explicit authorization for an exclusive disposable EC2 host, its isolated
PostgreSQL service and an exact S3 prefix, invoke:

```sh
AWS_REGION=<approved-region> HELMR_TEST_DATABASE_URL=<disposable-postgres-admin-url> \
  bash /candidate/run.sh /candidate /var/tmp/ne/run1 \
  s3://bucket/_verification/native-execution/operation VERIFIED_INSTANCE_ID
```

The host needs Nix, x86 KVM, cgroup v2 with CPU/memory/PID controllers, the existing
`helmr-verifier` system identity, unassigned jailer UID/GID 61001, and free
`/dev/nbd0` and `/dev/nbd1`. The runner verifies the EC2 DMI identity and rejects
active Product services or occupied network pools. It creates one owned cgroup
parent and supervisor leaf; the worker enters that leaf at process creation.
PostgreSQL gets a uniquely named temporary database through `dbtest`. The test
creates disposable credentials and writes objects only below the supplied
qualification prefix. Remote objects and evidence remain for inspection.

A foreground ordinary Computer Command runs the synthetic model on guest
loopback, outside Session scopes. A readiness Command validates its ports and
HTTP listener. External guest IPv4 traffic is blocked, with a loopback DNS
resolver; no provider credentials are used. A third authored Session continuously
writes a counter and emits output while both native harnesses complete two Turns.
The HTTP publication gate holds each native save before forwarding it, requiring
the owning Turn to remain `finalizing` without a completion receipt while peer
counter advances on five separate observations. The SDK then requires Completed, distinct own-save IDs and
unchanged native/setup identities. Database assertions join each completion to
that Turn's captured root digest and publication receipt, allowing recorded payload
retirement after physical closure, and check the four native MCP
enqueue effects with the exact native Session callers and Session-scoped MCP
provenance (no originating Turn ID). An ordinary Command reads the authored files
and compares them with the native and peer results. The driver answers only the
expected synthetic Claude enqueue approvals through the public Ask API; it checks
one approval per Claude Turn and none for the preapproved Codex tool.

Each provider's first Turn runs alongside SDK event iteration and approval handling.
After consuming public output, the driver aborts its iterator and creates a fresh
client. With publication held, it retrieves the same output and finalizing status,
enqueues the second Turn, and resumes events strictly after its last consumed
sequence. A bounded fixture-file handshake lets the gate independently confirm
the exact Session/Turn/Save and queued, unstarted successor before publication.
This exercises client detachment without resubmitting the first Turn. Healthy
continuation and checkpoint-fault drivers retain their separate gate behavior.

Each provider's second Turn is enqueued before the first completes. The publication
gate reads the authored JSON output through the public HTTP API, compares it with
the recorded result, and requires
the follow-up to remain queued while its predecessor is saving. The driver checks
that the follow-up starts after the preceding terminal timestamp. It retains each
completed Turn's admission ACK time and event-derived queue-to-running,
running-to-first-output and finalizing-to-completed intervals, plus the next
dispatch gap. These are one-sample phase measurements per Turn, not latency
distributions or a measurement of the complete handler-return-to-settlement interval.
The finalizing interval explicitly includes the deliberate publication hold; the
gate records that hold separately in `publication-gates.json`, keyed by save ID.
Per-Turn evidence is also written to the driver log so later failures preserve it.

Set `HELMR_NATIVE_PROCESS_LOSS_PROOF=1` to select
`TestWorkerAgentNativeProcessLoss`. After two completed native Turns per provider,
the authored fixture waits for an ordinary message during a third Turn, kills its
exact owned launcher-proxy handle and observes its exit. The unchanged guest
launcher must stop and join the actual native process after that connection loss.
This is proxy loss between native calls, not a VM loss or a failure during a
provider response. No guest diagnostic PID becomes a signal target and no
production fault-injection API is added.

Proxy exit may precede guest stop/join. The fixture records the actual existing
hold reason: `native_continuation_lost` when initial convergence succeeds, or
`native_convergence_failed` when it cannot yet be proved. The latter qualifies
conservative recovery through whole-process stop, not successful initial native
convergence. The test does not add a sleep or weaken the guest's physical checks.
The successor must remain queued under a visible local recovery hold while the
shared peer continues producing new output and setup remains at one. The driver
then explicitly resumes that exact hold. The next Turn must use a new logical
process/native scope and setup invocation, retain the native conversation ID and
both earlier input markers in model-request history, and complete with its own
published Save. Earlier completed results/Saves and the interrupted Turn remain
unchanged. SQL checks confirm the exact recorded hold, new process epoch and
dispatch after both hold release and old-process physical fencing. Request counts
reject replayed native work. This case does not prove
healthy RAM continuation or remote-only history recovery.

Teardown cancels the fixture's Sessions and Commands through public APIs, requests
Computer deletion and waits for physical lease/preparation stop acknowledgements
before terminating the worker. The CP lifecycle loops remain available throughout
that cleanup. SQL reads discover only this fixture's Environment objects.

Success requires both the test PASS and the outer process, NBD, cgroup, mount and
network cleanup audit. On failure retain the working directory and inspect the
reported differences; do not infer cleanup from a worker exit or remove an
unidentified resource. The baseline case does not qualify source-loss restoration,
cross-host continuation, encryption of RAM snapshots, external provider egress,
protected HTTPS substitution, background-save cadence, or the full release.

For the separate Worker authority-loss case, set
`HELMR_NATIVE_AUTHORITY_LOSS_PROOF=1` on the same authorized runner invocation.
Do not combine it with the publication-owner restart mode. The runner first
qualifies read-only NBD idle/exclusive-claim observations on both dedicated
devices, then runs `TestWorkerAgentAuthorityLoss`. This case uses the authored
peer Agent and no native model. It kills only the Worker while a mandatory Save
is either captured but unpublished, or committed with its response withheld.
The fixture records live helper/consumer identities and retains pidfds before
the fault. Restart must refuse admission while preserving the arena; the CP must
settle publication loss without releasing physical charges. Only after consumer
cessation and definitive Save disposition does the fixture signal its known
helper, verify device inactivity and remove its exact arena. Ordinary restart
must then activate and complete a fresh Turn using the released device, without
replaying the lost process. A final owned marker tests drain refusal, followed by
clean drain completion after that fixture-only marker is removed.

The per-case `authority-loss.json` and Worker logs separate these observations.
They do not prove cross-epoch publication, transparent customer-process recovery,
automatic orphan reclamation or systemd restart behavior. A failed identity,
disposition or physical-absence check retains uncertain resources for explicit
operator audit; a remembered PID or an absent marker never authorizes cleanup.

## Component continuation qualification

Run from the repository with Docker available:

```sh
nix develop --command tests/build/native-continuation.test.sh
```

This script is an explicit manual qualification; ordinary Go tests skip it.

The disposable privileged Linux container runs managed Node with the pinned Codex
binary and Claude Agent SDK. Each Session retains its native conversation through
whole-Computer freeze, source-abort authority installation, replacement transport
attachment to the same owner, and activation. Assertions compare setup nonce, managed/native process IDs, native
model request history and the authority generation of MCP enqueue calls before
and after continuation. Native clients receive their MCP connection once in setup.
The Codex fixture preapproves only the synthetic enqueue tool; Claude approvals
receive a synthetic affirmative response from the local operation responder.

The model server and operation responder are test infrastructure. No provider
credentials, external model calls or real peer work are used. This is evidence for
Linux process/cgroup, native-client and guest transport behavior. It does not prove
Control Plane admission, durable activation, VM serialization, remote-only restore,
native restore-path continuity, or release artifact verification. The test uses the Docker host's native CPU to
exercise required Linux syscalls; its runtime metadata has the product's x86_64
architecture declaration. Actual x86_64 release artifacts require KVM qualification.

The model server runs outside authored Session scopes: placing an HTTP server in
setup correctly fails the runtime's unqualified asynchronous-resource check.
Responses use the namespace field advertised by the pinned native client, following
[function tool calls](https://developers.openai.com/api/reference/typescript/resources/responses/methods/create).
The fixture's per-tool Codex approval setting follows the
[configuration reference](https://developers.openai.com/codex/config-reference).

For an explicitly authorized disposable x86 KVM host, the separate
`TestNativeComputerKVM` fixture captures RAM and both writable disks, publishes to
an isolated S3 prefix, closes the source VMM/NBD owner, deletes its mutable working
directory, downloads snapshot files by checked digest, and restores a fresh VM.
It advances physical owner, writer, lease and Session authority before thawing.
Its native assertions require the same setup/PIDs and the second MCP reply in
retained conversation history. The Control Plane responder is synthetic; this
is not durable CP activation or release certification. A combined invocation uses
one physical host; the split operation below requires independent host evidence.
These host Session library functions are not yet wired into the production worker
owner; this fixture does not qualify an integrated worker flow.

Prepare fixture artifacts locally from an explicitly selected, existing x86
image containing `/opt/helmr/runtime`:

```sh
HELMR_NATIVE_RUNTIME_IMAGE=<local-image> nix develop --command \
  tests/build/native-continuation-artifacts.sh /absolute/new/output
```

The helper records the immutable image ID, validates its Runtime metadata and
rebuilds runtime entry modules with the product esbuild script and pinned Node
target from the current checkout. It reuses the image's
Node and libraries; it does not rebuild or certify a release. The resulting
Computer seed, Runtime/Program squashfs and model script are checksummed.
For a complete candidate, run `tests/build/native-continuation-package.sh` in
`nix develop .#images` with the same image variable and a new absolute output
directory. It requires a clean commit, rebuilds the fixture, current boot artifacts,
NBD worker helper and test binary, archives that commit, and records all hashes.
It rejects source changes during the build. `native-continuation-kvm.sh` expects that
candidate and runs only after the host/S3 operation is authorized. Choose a short
absolute evidence directory such as `/var/tmp/native-ab12/run1`: Linux Unix socket
paths, including the jailer suffix, must fit in 107 bytes. It rejects
active Product services and occupied NBD devices or address pools, retains test
logs, and reports cleanup discrepancies. The operator must inspect the resulting
process, route, network and NBD evidence before cleanup or stopping the host.

The host model is loopback-only. The fixture's existing transparent egress adapter
maps only the two advertised ports at `203.0.113.1` to those loopback listeners;
a fixture-only forward-chain drop in each owned guest network namespace blocks
all other egress, including DNS, before native startup or restored activation.
No provider credentials or Internet model
requests are involved. Invoke the model with `203.0.113.1` as its final argument;
the KVM runner does this and joins that exact model process on exit.

For the exact-owner cleanup check without KVM, manually run the real kernel cgroup
case in a disposable privileged Docker namespace (the Docker server's native CPU):

```sh
nix develop --command bash -seu <<'SH'
  out=$(mktemp -d)
  trap 'rm -rf "$out"' EXIT
  arch=$(docker version --format "{{.Server.Arch}}")
  GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go test -c ./internal/firecracker -o "$out/test"
  docker run --rm --platform "linux/$arch" --privileged --cgroupns=private \
    -e HELMR_PRIVILEGED_CGROUP_TEST=1 -v "$out/test:/test:ro" node:bookworm \
    /test -test.run "^(TestJailerCgroupPath|TestCleanup.*)$" -test.v -test.count=1 -test.timeout=60s
SH
```

`TestCleanupExactCgroup` must execute and pass; a skipped case is no kernel proof.
It checks that populated/child groups block cleanup without losing the ownership
marker, and that retry removes only the exact empty owner group. The shared jailer
parent belongs to host/fixture lifecycle, not individual VM cleanup. Existing
unmarked cgroup residue from an older runtime requires inspected operator cleanup;
the runtime does not infer deletion authority from a directory name.

Run `nix develop --command tests/build/native-continuation-model.test.sh` to check
that a fresh model process preserves the saved ports and prior request sequence.

The KVM runner also supports an explicit two-host operation using the same clean
candidate on each host:

```sh
# Source host: host identity must be verified against the infrastructure inventory.
bash /candidate/run.sh /candidate /var/tmp/ns/source s3://bucket/_verification/native-continuation/operation source SOURCE_INSTANCE_ID
# After source success, cleanup audit and source EC2 stop, carry only the small
# continuation.json.object.json descriptor to the independently verified target.
bash /candidate/run.sh /candidate /var/tmp/nt/target s3://bucket/_verification/native-continuation/operation restore TARGET_INSTANCE_ID /receipt/continuation.json.object.json
```

Source mode closes and joins its VM/device owners, removes its mutable working
directory and publishes continuation metadata in the same remote CAS as the disk
and RAM. The metadata contains only fixture state: snapshot descriptors, original
synthetic operation receipts, setup/PID assertions and the synthetic model's
history/ports. The target fetches it by digest and size from the approved remote
prefix; it does not copy a source disk, RAM file or working directory. It binds
those same model ports or fails, preserving the setup-created endpoint. The
second native call must retain the first call's history and use the advanced
Session/lease authority. Split mode checks each host ID against its local DMI board asset tag and requires
different IDs; the operator
must independently verify those IDs and source shutdown. A source-only PASS is
not a complete continuation verdict, and a failed outer cleanup audit must be
resolved before target execution. Both runs need explicit infrastructure authorization. Source metadata publication is a fixture artifact, not durable CP activation.

The local native MCP path can also use the complete CP HTTP server and a real
PostgreSQL fixture through Docker Desktop's host route:

```sh
HELMR_NATIVE_CP_PROOF=1 nix develop --command go test ./internal/controlplane -run '^TestWorkerAgentNativeManagedMCP$' -count=1 -v
```

The opt-in starts disposable privileged containers. Test-only host credentials
and the TLS server certificate are mounted into that container; the client trusts
only that certificate and validates its server name. Both native clients enqueue
through the guest-local MCP bridge, production host Session service, authenticated
worker client and CP transaction. Four persisted Turns, unchanged local MCP
URLs/headers, setup nonces and native PIDs are required across credential expiry
and automatic guest authority renewal. The 35-second host TTL exceeds the client's
30-second refresh margin; later MCP calls after a 36-second idle interval must use
a different credential. A second run cancels the active fixture and checks that
its container, image and temporary files are removed. Model responses and non-MCP authored
operations remain synthetic. This does not qualify durable Turn finalization,
VM serialization, cross-host CP adoption, or the server-401 credential retry path.

The authority-loss cases retain their isolated PostgreSQL databases and mode-0600
`recovery-private.json` CP restart inputs even after passing. Test teardown closes
connections but never drops these databases. Keep these protected inputs on the
owned host; exclude them from shareable evidence archives. An operator may remove
the exact database and protected input only after definitive Save disposition,
physical reconciliation and the independent outer cleanup audit all succeed.
Stopping the private PostgreSQL service preserves this recovery state on disk.

## Healthy Full-RAM continuation

`HELMR_NATIVE_HEALTHY_RAM_PROOF=1` selects
`TestWorkerAgentHealthyContinuation` through the same production package and
Worker launcher. It is exclusive with the fault/restart modes. The synthetic
loopback providers run in a completed `models` Session's setup state, so an
unreconciled long-running Computer command does not prevent legitimate capture.
An active peer still proves online post-Turn saves before each idle boundary.

The driver first closes the model Session's authored HTTP listeners, which would
otherwise correctly pin compute as unqualified asynchronous resources. It then
releases the peer and waits for read-only PostgreSQL observations of
a remotely ready checkpoint and the ordinary Worker's physical source-stop
receipt. It waits 65 seconds after the first release (past grant and transport
lifetimes), then starts the next peer/native Turns through the public SDK. Two
cycles require setup count one, the same native conversation, Session/native PIDs,
nonce and native scope, advancing in-memory counters and retained model request
history. PID equality is supplementary observation: these PIDs are namespace-local
and may be reused after restart. Retained random setup nonces, native conversation
and scope identities, and advancing in-memory state establish continuity together.
The model Session reopens its same ports after restoration. Both native
harnesses must complete managed MCP calls after each restore. Prior Completed Turns and their own Saves remain
unchanged. The final database checks bind both restores to coherent published
cuts, new physical instances and lease epochs, source fencing before delivery,
rotated channel credentials and reconciled controls.
Runtime object pins are observed while the checkpoint is ready, then matched to
the retained manifest at final verification. Normal retention may release these
pins after both physical instances close; the fixture does not require historical
checkpoints to retain unnecessary object pins.

`checkpoint-observations.json` is a read-only fixture observation, never an
allocation or fence instruction. `healthy-continuation.json` retains the actual
source/target host and instance identities plus publication/fencing timestamps
and runtime object sizes. A single-host run qualifies this bounded release and
restore path only; it does not establish distinct-host restore, hours-long idle,
provider credential expiry, or the full failure/performance campaign.

## Two-hour idle continuation

`HELMR_NATIVE_LONG_IDLE_PROOF=1` selects `TestWorkerAgentLongIdleContinuation`,
exclusive with the other modes. It reuses the healthy same-host case, with a
minimum two-hour first idle interval after remote readiness and source fencing.
Every 30 seconds the driver requires a fresh read-only checkpoint observation,
ready status, no unfenced Computer lease and unchanged prior Completed Turns.
Final SQL also requires consecutive lease epochs and database timestamps bracketing
the two-hour idle interval, ruling out a restore between samples. The driver records
requested and measured idle time, wall-clock boundaries and sample count separately
from resume time. The second cycle retains its five-second idle.
Both restores retain the healthy native/setup/history and managed MCP assertions.
The driver, case and outer runner bounds are 130, 135 and 139 minutes respectively.
This mode requires a separately qualified live run; adding it is not runtime proof.
Synthetic providers do not establish real provider credential expiry behavior.

## Checkpoint key mismatch

`HELMR_NATIVE_CHECKPOINT_KEY_MISMATCH_PROOF=1` selects
`TestWorkerAgentCheckpointKeyMismatch`, exclusive with the other modes. After
the healthy fixture's first ready checkpoint and physical source fence, the
fixture joins its original Worker and starts the same retained host identity
with a different fixture checkpoint key. A new peer Turn requests restoration.
The production Worker must identify one key mismatch, close failed allocation
resources, remain running with both admission reasons set to
`checkpoint_key_unavailable`, and preserve the ready checkpoint's four retained object references.
For twenty seconds, the peer input remains queued, Sessions have no recovery hold,
prior Completed Turns stay unchanged and no additional lease is allocated.
New allocations may occur before the next ordinary Worker observation reports
the pause to the Control Plane. Each must physically close before the stable
interval starts; the fixture records their count and final epoch.

The fixture then joins that Worker and restarts it with the original key through
ordinary startup. No database mutation clears the pause or changes checkpoint
state. Both native harnesses complete the retained healthy continuation checks,
including setup count one and managed MCP operations, followed by a second normal
release/restore. Final SQL binds every failed lease to the original disk Save,
requires physical closure without guest initialization, and checks the corrected
Worker's next lease consumed the original RAM cut. Worker replacement and fault
receipts are fixture-owned; logs never include the key bytes. Driver/case/outer
bounds are 15/20/24 minutes, with normal retirement and physical audit required.
If the driver fails while the wrong-key Worker remains paused, normal retirement
may be unable to restore and close retained Sessions. Preserve that failure and
complete the explicit operator physical audit, fixture cleanup and host stop;
operator closure does not turn the test or normal retirement into a pass.
This case needs live qualification. It does not qualify hot rotation, permanent
key loss, mixed-key fleet availability or real provider credential expiry.

## Lost checkpoint qualification

`HELMR_NATIVE_CORRUPT_CHECKPOINT_PROOF=1` selects
`TestWorkerAgentCorruptCheckpoint`, exclusive with every other fault/restart mode.
It uses the same four Sessions and first four native Completed Turns as the healthy
fixture. After the source is ready and physically fenced, the test flips one byte
of its encrypted VM configuration in the isolated, versioned S3 qualification
prefix. The original bytes, digest, size, ETag and version are checked before a
conditional write; both object version IDs and the observed replacement digest are retained.
It does not modify the manifest, PostgreSQL state or production Worker.

`HELMR_NATIVE_MISSING_CHECKPOINT_MEMORY_PROOF=1` instead selects
`TestWorkerAgentMissingCheckpointMemory`. After the same normal ready/source-fenced
boundary, it checks the published memory object's current size, ETag and version,
then conditionally creates a delete marker at that exact digest key. A subsequent
S3 HEAD must report `NotFound` before any new input is admitted. The original object
and delete-marker version IDs, memory digest and explicit absence observation are
recorded. This injection does not download or modify the memory payload and does
not permanently delete its version; ordinary reclamation can subsequently do so.
It is exclusive with every other fault/restart mode.

Both cases use the same assertions. The SDK admits two new native Turns and
observes local holds on all four Sessions.
For at least 20 seconds, both inputs remain queued, hold identities stay stable,
and prior Completed Turn records and their completion-save references remain
unchanged. SQL then requires
one physically closed restore attempt, a definitively lost checkpoint, no guest
initialization or reconciled controls, and only the original four process epochs.
`checkpoint-loss.json` retains the physical lease and process observations.
The driver cancels Sessions only after recording these observations, and ordinary
fixture retirement performs the remaining cleanup. Neither case claims
corruption within the memory payload, transport-failure, incompatible-runtime or
explicit-resume proof.


## Integrated distinct-host continuation

`TestWorkerAgentCrossHostContinuation` extends the healthy two-cycle case through
actual CP and Worker owners. It requires separately authorized source and target
instances, exact physical identities, the same checksum-verified candidate on
both hosts, and an explicit private loopback transport from the target to the
source CP. Infrastructure provisioning and that transport belong to the operator;
this test does not create hosts, tunnels or credentials outside its fixture.

On the source, pass `HELMR_NATIVE_CROSS_HOST_TARGET_ID` with the independently
verified target instance ID to the ordinary four-argument runner. Do not combine
this case with any fault selector. The runner checks its own DMI identity and
uses the actual source instance ID as its Worker resource ID. After the first
ready checkpoint and physical source-VM stop, the driver requests a handoff.
The test joins the source Worker and writes `proof/cross-host-ready.json`, which
contains the exact CP loopback URL, checkpoint, resource and runtime identities
without secrets. The separate mode-0600 `cross-host-bootstrap-private.json`
contains fixture enrollment and encryption inputs. Transfer that file directly
through authenticated pinned transport to an exclusive private target file;
never print, hash, publish or collect it. Delete the source copy only after the
target installation acknowledges success.

Before starting the source case, launch the target runner in a named transient
systemd unit with redirected private output and retained exit status, passing a
new bootstrap path that does not yet exist. Restart and preflight the source
EC2/PostgreSQL before starting this target wait:

```sh
AWS_REGION=<approved-region> bash /candidate/run.sh /candidate /var/tmp/nt/run1 \
  s3://bucket/_verification/native-execution/operation VERIFIED_TARGET_INSTANCE_ID \
  target /private/cross-host-bootstrap-private.json
```

Run the source in its own named transient unit too, so an SSH disconnect does
not send either runner SIGHUP. Record both unit identities and inspect their
terminal status rather than relying on a live SSH connection. Use a passwordless
local PostgreSQL socket URL or a root-private environment file; never place a
password-bearing URL in unit properties. Give the units no runtime kill timer:
normal shutdown signals the recorded Worker through its verified pidfd, because
the Worker lives in the fixture cgroup outside the unit. Stopping a running unit
is not a physical cleanup operation. The target uses
the same preflight, immutable manifest verification, delegated
cgroup and before/after physical inventory as the source. It requires a fresh
Worker workroot and verifies its DMI/resource identity against the bootstrap.
After preparation it atomically writes `target-prepared.json` and waits up to
20 minutes for the private bootstrap. Wait for that receipt before starting the
source case immediately. The 20-minute target wait allows the measured roughly
10-minute full healthy fixture, the five-minute handoff bound and preparation
margin; the actual wait is recorded separately. The operator then establishes and checks the loopback bridge from
the observed source URL, and installs the bootstrap atomically without replacing
an existing file. Only immutable candidate artifacts are transferred beforehand. All mutable disk
and RAM must come through the ordinary remote restore path. The target removes
its transferred bootstrap after reading it and starts the unchanged Worker;
`proof/target-start.json` records the process and physical-resource identity.
It remains running until the operator sends its verified process SIGTERM.

The source handoff waits at most five minutes after the driver request, requiring
an active compatible target and expiry of the source's actual observation under
the allocator's 120-second freshness rule. Pool activation enforces compatibility; refused qualification remains bounded
and retains the last target status plus its Worker log for diagnosis. It writes
phase times and the exact checkpoint receipt before the driver enqueues new work. The complete case has a
45-minute driver bound, 50-minute fixture context and 57-minute outer test bound.
Cross-host retirement has a separate five-minute bound. These bounds include
the additional control-plane round trips through the operator transport; they
do not establish a regional performance target.
Ordinary healthy assertions remain, plus SQL requiring the first continuation
source-to-target and the second target-to-target with matching VM/CPU identities.
A provider inventory and both DMI receipts establish distinct physical placement;
logical Worker IDs alone do not.

Keep both tunnel connections alive through ordinary Session/Computer retirement.
`proof/retirement-complete.json` records successful retirement; then stop/join the
target Worker and require its outer audit, separately from the source audit.
If retirement or transport fails, retain the database, `recovery-private.json`,
protected host state and exact bootstrap paths for operator reconciliation.
Restore the same loopback bridge for cleanup before retrying ordinary retirement.
Do not drop the database or infer physical absence from an interrupted test.
After both audits pass, remove the exact temporary enrollment/bootstrap/recovery
inputs, stop the private PostgreSQL service, and perform the approved host cleanup.

Collect only explicit nonsecret filenames: source/runtime manifests, execution
and Worker/driver/CP logs, ordinary result/publication/save observations,
`cross-host-ready.json`, `cross-host-continuation.json`,
`checkpoint-observations.json.handoff-receipt`, `retirement-complete.json`,
`target-prepared.json`, `target-start.json`, and the named before/after inventories. Exclude all config,
enrollment, host-secret and `*-private.json` files. Source-only success is not a
cross-host verdict. This case does not prove source-EC2 outage, real provider
sign-in, arbitrary platform compatibility, hours-long idle or performance distributions.


### Cancellation during native finalization

Set `HELMR_NATIVE_FINALIZATION_CANCEL_PROOF=1` to select
`TestWorkerAgentFinalizationCancellation`. It uses both real native harnesses,
the ordinary public Session cancellation API and a publication gate on the third
Turn of each Session. Two prior Completed Turns retain their exact results and
own Saves. While the third Turn's captured Save remains unpublished, the driver
enqueues a fourth Turn and cancels the Session. Both unsettled Turns must become
Cancelled; the queued Turn must never start, the public prepared result must stay
unavailable and the active peer must continue producing output.

The fixture then forwards the original Save publication unchanged. The real Save
must publish, while the Turn remains Cancelled with no completion Save or success
event. SQL checks run before forwarding, after publication and at case completion.
Prepared result/capture evidence remain internal; cancellation does not roll back
written data. This case does not qualify configured Turn deadlines or revocation.
It uses the ordinary 10-minute driver / 15-minute case bounds and normal retirement.

### Configured Turn deadline during finalization

Set `HELMR_NATIVE_FINALIZATION_DEADLINE_PROOF=1` to select
`TestWorkerAgentFinalizationDeadline`. After the two native harnesses have completed
their ordinary Turns, an authored Agent with `maxTurnDuration: "30s"` returns a
result on the same Computer. The fixture holds its captured Save publication
before the configured deadline. A queued successor remains accepted but cannot
run after natural deadline settlement places one Session-local runtime hold.
The driver observes active peer work while waiting; it does not mutate database
clocks/state or call cancellation to cause expiry.

The original Save then publishes. The expired Turn must remain Interrupted,
without public result, completion Save or success event, and the successor stays
queued with the same hold present across five fresh peer progress samples.
Prior native Completed results/own Saves remain
unchanged. Finally, ordinary fixture cancellation settles that retained queued
input. SQL independently checks deadline/capture/terminal ordering before and after
publication, and again after cleanup. This qualifies the shared Turn/Save deadline
path with a real guest; it does not model native-provider credential expiration.

### Background recovery saves with active work

Set `HELMR_NATIVE_BACKGROUND_SAVE_PROOF=1` to select
`TestWorkerAgentBackgroundSaves`. The fixture requires its 30-second save interval
and holds the peer Turn open until three distinct optional Saves publish. Each
observation checks the current recovery head, live lease, absence of a Turn or
checkpoint link, and retained Save/root counts and pinned object bytes. The peer
counter must advance between publications. The ordinary native Turns and their
individual completion Saves are then exercised unchanged.

Combine this with `HELMR_NATIVE_RESTART_PUBLICATION_OWNER=1` to exercise the same
background work through the existing capture, partial-upload and publication
restart fault windows for native Turn Saves. Background Saves pass those hooks
without being mistaken for Turn finalization.

`background-publications.json` contains the three database observations, and
`background-before-native.json` records the driver's running-peer assertions.
This is a light-write periodic recovery test. It does not establish a capture
latency limit or write-heavy retention growth over long leases. Every retained
lease can accumulate Save rows and object pins even when disk content is unchanged.


Set `HELMR_NATIVE_CHECKPOINT_READ_RETRY_PROOF=1` to exercise two transient
checkpoint-read response failures during the first healthy restoration. The
fixture first forwards each request to the real authenticated Control Plane,
then replaces successful manifest responses with HTTP 503 for two successive
allocations. Ordinary retry must physically close both failed allocations before
restoring the exact same checkpoint on the third. The case preserves the manifest
digest, published disk identity, native setup/history/in-memory identities and
all existing two-cycle healthy-continuation assertions. Evidence is written to
`checkpoint-read-retry.json`. This is a bounded Control Plane response fault;
it does not simulate an S3 outage, corrupt bytes or change deployment compatibility.

Set `HELMR_NATIVE_SESSION_DISCOVERY_PROOF=1` to select
`TestWorkerAgentSessionDiscoveryContinuation`. The peer creates one owned and one
independent receipt Session through its authored Runtime SDK. Both initial Turns
complete; the parent closes the owned child, and the external fixture principal
closes the independent Session after verifying requester identity. Requester
origin does not grant lifecycle control. Their closed records remain discoverable.
The peer retains SDK identity handles and its managed MCP descriptor in setup
state. Before hibernation and after each of two actual RAM restorations it inspects
both records, paginates owned/requested results, and checks the same initial Turn
IDs/statuses through SDK and MCP. Before closing the owned child, the peer observes
its guest-written active-handler marker, interrupts it with a replay-stable
receipt, requires its successor to remain queued under the exact hold for at least twenty
seconds, then
resumes that hold and requires successful successor completion. Replaying the
original interrupt must not recreate the released hold. A separate owned child
checks runtime cancellation of running and queued work. These are authored SDK
controls; managed MCP only observes their state. The driver requires three
observations with unchanged discovery identities and fresh reads of all four
controlled Turn outcomes. SQL independently binds successor dispatch to hold
release and old-process fencing, requires a replacement process epoch, and checks
physical closure of the cancelled process with its queued input never started.
The held observations are a bounded live interval, supplemented by the deterministic
control-owner tests; they are not a scheduler latency bound. Existing native/setup/history and four-member checkpoint
assertions remain in force; this case does not claim setup-time MCP recovery or
independent-target lifecycle authority.

Set `HELMR_NATIVE_SECRET_BINDINGS_PROOF=1` to select
`TestWorkerAgentSecretBindingsContinuation`. Its disposable API key additionally
grants `computers.create` and `secrets.write`. The driver creates three random
Secrets and binds raw env, file, and protected env to one Computer through the
ordinary SDK. The authored peer checks raw/file digests and a protected selector
before hibernation and after both RAM restorations. Values and selectors are never
written to evidence. Rotation while the first checkpoint is released must not
rewrite the admitted process values or its retained selector.

After both native harnesses complete their eight own-Save Turns, revoking the raw
Secret must interrupt the active peer, leave its queued successor unstarted and
inspectable under a hold, and permit explicit Session cancellation of that input.
Earlier native outcomes must remain unchanged. `result.json` records the three
checks, Secret identities and revocation/cancellation observations. The ordinary
outer retirement still checks physical closure. This case does not exercise
protected upstream requests, provider OAuth, or erasure of delivered values from
snapshots.

Typecheck the authored and external-driver fixture with
`bun x tsc --noEmit -p tests/fixtures/native-continuation/tsconfig.json`. Its SDK
paths use one source type identity for the fixture and imported native examples;
the execution package still builds the application through the ordinary compiler
and its packaged SDK.

The expanded discovery/control case uses 15-minute driver, 20-minute fixture and
24-minute outer test bounds. These include the sequential held interval, process
replacement and cancellation before two RAM cycles, rather than setting a product
latency budget. After assertions end, fixture retirement forwards requests directly
to the production CP handler; retained failure evidence is unchanged, and a failed
driver never becomes success through cleanup.

For matched saving measurements, build a separate fixture with
`HELMR_NATIVE_PERFORMANCE=1 tests/build/native-execution-package.sh <candidate>`.
The image includes a checksum-bound repository archive, locked build tooling and
an offline npm tarball cache. This variant's Worker is built with `computerproof`
to observe storage HTTP client calls below SDK retries. Ordinary Worker builds
have no observer or measurement flag. Its receipt explicitly identifies the
instrumented variant; do not substitute an ordinary native fixture bundle.

Run this variant with `HELMR_NATIVE_PERFORMANCE_KIND=no-op`, `edit-test`, or
`dependencies`, selecting `HELMR_NATIVE_PERFORMANCE_BACKGROUND=0|1` and
`HELMR_NATIVE_PERFORMANCE_DEGRADED=0|1`. Each invocation needs fresh evidence and
remote prefixes under the existing authorized host operation. It retains the
healthy fixture's two native providers, queued successors, two RAM continuations
and eight distinct own-Save completions. Native tools perform the workload, and
the model follows yielded command handles until exit. Synthetic local responses
exclude external model latency. The ordinary physical-retirement audit still
applies to every invocation, including failures.

The no-op reads input identity without editing repository/dependency files;
native conversation, result and peer files still change. The first edit/test
extracts a full repository copy, changes the SDK Content-part boundary, builds
that module and checks five boundary/validation assertions. Subsequent Turns
edit and test the retained copy without extraction. This is a representative
module build/test, not the whole repository test suite. Dependency Turns replace
the installed tree with a real `npm ci --offline` from the locked tarball cache.
They measure fresh dependency writes, not network package downloads. Workload
receipts separate operation time from subsequent footprint traversal overhead.

Final-only cases use an ordinary positive one-hour saving interval, longer than
the bounded case, and require zero optional Saves. Background cases use five
seconds and require actual optional Saves. Both have an identical twelve-second
peer I/O warm-up outside native handler timing. Measured requests bypass the
functional fixture's artificial publication holds. In degraded cases, the first
uncertified segment of each native required Save encounters two seconds of
request-scoped CAS metadata unavailability, starting at its first actual `Stat`.
The exact Save/digest and monotonic deadline survive retries; subsequent successful
certification is required. This is not bandwidth throttling or a whole-store outage.

Client receipts retain monotonic start/enqueue-to-ACK, first observed running
state/output, and settlement observation for each native Turn, with raw Session
events. For both native providers, this fixture's first useful output is the
validated workload result, authored after native execution and before the outer
handler returns and begins mandatory saving. The measured request-to-result time
includes native operation completion; it does not measure token/commentary
streaming. Client polling may observe this output after settlement. Output
observations include HTTP/polling/ask-handling delay (the native
loop waits up to 200 ms between checks); they are not exact emission times.
Successors are admitted immediately while their predecessors are active. Their
ACK measurements do not assert overlap with the later storage fault. Separate
same-PostgreSQL wall-clock lifecycle timestamps retain admission-to-dispatch and
predecessor-settlement-to-dispatch intervals, without subtracting client clocks.
Initial peer admission-to-output includes Computer preparation/startup. After a
released checkpoint, peer input-to-first-output is recorded separately from idle
duration, model reopening and subsequent native workloads.

Evidence includes Runtime handler-return-to-observed-settlement diagnostics,
storage fault/recovery windows, every Save's classification, peer 32-KiB buffered
write/read latency, and scoped resource observations. Runtime timings include
transport/processing delay before the Runtime sees the authoritative outcome;
they do not claim the remote commit instant. Diagnostics are best effort in the
product, but the performance case requires a complete set with intact byte custody.
They use the internal process-diagnostic stream; they are not public Turn output.
This benchmark consumes its fixed qualification application's diagnostics, not an
authenticated measurement API for arbitrary authored programs.
The proof-tag observer runs in the Control Plane test child in every functional
mode; only performance packages also instrument the Worker. Ordinary production
binaries do not include this observer. It records SDK operation/invocation/attempt
identity, store namespace and content digest, and requires matching certification
observations for all native Saves. Storage counters report client body bytes read, including SDK retries and replayed
bodies, rather than confirmed wire delivery or HTTP/TLS overhead. Request
durations include consumption of the observed response body. Local/remote
inventories are sampled every two seconds when observation cost permits; report
sampled maxima, sampling gaps and incomplete rows. Missing observations or an
incomplete sample during the driver interval fail the measurement gate while
retaining the raw evidence. This availability check does not prove continuous
coverage or physical peaks. Worker cgroup, owned Firecracker cgroups/processes,
Control Plane process readings and whole-host readings have distinct scopes.
VM process CPU and resident/high-water memory are retained even when optional
jailer-parent memory/I/O controllers are unavailable. Only VM owners from the
case's custody directory are observed; launching/unreadable owners and disappearing
processes are recorded as unavailable rather than fabricated zero usage. Local and remote inventories are also
recorded after retirement, before the operator's final isolated-prefix cleanup.
Inventory overhead and its remote page calls remain separate from measured
Worker/Control Plane traffic. Preserve raw samples and report sample counts and
ranges; a small number of repetitions does not establish tail percentiles or a
release latency budget.
