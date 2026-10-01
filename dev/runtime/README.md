# Dedicated runtime host

This is a dedicated development verification profile, not a general deployment target.
It composes the normal Control Plane, Dispatcher, PostgreSQL 18, Redis,
ClickHouse and installed Worker on one dedicated Linux x86_64 KVM/systemd host.
A passing `start` means CP and native Worker readiness, Redis PING and active
service processes only. On 2026-09-27 the Task, same-host persistence and Actor
continuation cases passed on a disposable host using normal authentication and
private native artifacts. Separate checks also passed a changed CP executable,
an edited-initial-migration reset and guest IPv4 metadata denial. These do not
qualify arbitrary service changes, general network isolation or cross-host recovery.

## Inputs and ownership

One dedicated host serves one repair objective at a time. It may be stopped and
retained between objectives. Reserve sufficient disk and memory
for the real services, verifier and guest. The sample caps Computer staging at
4 GiB for these small cases; the native default is 64 GiB. Reserve space for
the live disks and checkpoint intermediates together. A 200 GiB pilot with the
64 GiB staging default ran Tasks but rejected checkpoint capture for capacity. A scope spans multiple correction attempts. Do not install this
profile on shared staging or a host belonging to another owner.

Provisioning must supply:

- KVM, cgroup v2, systemd with `DelegateSubgroup`, Python 3.12+, `runuser`, the
  shared Worker's OS packages and explicitly assigned, disconnected NBD devices.
  Load/configure NBD during provisioning, including native modules-load/modprobe
  configuration for reboot. Verify these devices again after restart; this profile never borrows live devices
  or repartitions a disk. Choose network pools, DNS and blocked destinations for
  the actual host network, including metadata and other privileged endpoints.
- The source-owned shared Worker installer and digest-verified host/runtime
  bundles described in [README](README.md). Run its `install` before installing
  this profile. Its systemd unit remains the Worker execution boundary.
- Real, isolated S3 CAS and platform storage with authorized native AWS credentials
  or a host role. The normal runtime release must already be privately published,
  with its actual descriptor. No file CAS, synthetic descriptor or public release
  is part of this path. This script does not grant AWS access or publish artifacts.
- A `build_services.py` candidate directory containing Linux CP/Dispatcher binaries,
  input digests and the clean archived source, plus the native Worker host/runtime
  bundle receipts from that initial source revision. These API-only service binaries
  do not embed the console; normal setup/API authentication is still required.
  Supply absolute paths to the
  backing services from the pinned toolchain. Provisioning must retain their Nix
  closure/profile if using Nix; a development shell alone is not a GC root.
  Supply the actual `/nix/store/.../bin/...` paths, rather than standalone symlink
  aliases: PostgreSQL locates its accompanying share files from its executable path.
- Normal OAuth configuration and a normal Worker enrollment token. Normal
  self-hosted setup and environment-scoped API keys are still required for cases;
  no seeded user or authentication bypass is installed.

Copy [config.example.json](config.example.json) to a private file and
replace its placeholders. `control_plane` and `worker` contain ordinary native
configuration, including their own AWS settings where needed. The script owns
local endpoints, default region/group/pool, store agreement, key generation and
service identities, and rejects overrides of those fields. Per-scope keys and
passwords are generated once on install and retained across starts. Secrets never
belong in source control or the evidence bundle.

Readiness waits stop immediately when their systemd service reports `failed`,
retaining its journal and state for diagnosis. A live process may still need the
normal readiness deadline; the profile does not restart failed services.

Keep the Runtime descriptor's canonical JSON bytes; pretty-printing an extracted
descriptor makes CP reject it. Bootstrap enrollment tokens use the native
`hlmr_wgt_` prefix followed by 32 random bytes encoded as unpadded base64url, not
a bare random hex string. Blocked IPv4 CIDRs must be unique and sorted by numeric
network address, then prefix length. The Worker also blocks its host interfaces
and link pool; include the actual metadata/privileged destinations explicitly.
Set `VM_SCRATCH_DISK_MIB=32768` before first enrollment: ordinary Workspaces
require 32 GiB of guest ephemeral disk. The Worker default of 8 GiB can become
ready while remaining ineligible for every ordinary Workspace. The pool seals its
shape on registration; changing this after enrollment requires normal pool
replacement or an explicit disposable profile reset, not a database row edit.
The sample allows 120 seconds for guest health during this feasibility run. A
cold real Workspace took about 46 seconds to report healthy on the pilot
host, exceeding the native 30-second default. This is a startup allowance, not
a boot-latency improvement or a performance target.

The profile uses self-hosted Computer key wrapping. Backing services and CP bind
loopback; access CP through an authorized tunnel. PostgreSQL uses a separate
non-superuser application role with SCRAM authentication over loopback, and
ClickHouse uses the real bootstrap command to create separate reader, ingester
and migration credentials. This does not prove managed TLS/IAM/KMS, provider
scaling, rollout behavior or cross-host restore.

## Commands

Offline rendering validates configuration shape and writes a new private directory:

```sh
python3 dev/runtime/host.py render \
  --config /private/runtime-host.json --output /private/runtime-host-render
```

After the provisioning and live-use authorization applicable to the scope:

```sh
sudo python3 dev/runtime/host.py install --config /private/runtime-host.json
sudo python3 dev/runtime/host.py start
sudo python3 dev/runtime/host.py inspect
sudo python3 dev/runtime/host.py stop
```

`install` requires a fresh dedicated host and an inactive Worker. It copies the
application binaries and runtime descriptor into `/etc/helmr/verification`,
records binary digests, creates the backing-service identity and database cluster,
and writes the units. It does not start services. Keep the source bundle/ref,
artifact manifests and rendered candidate receipts with scope evidence; binary
hashes alone do not make source recoverable.

`start` starts the backing services, creates the local application database,
executes native ClickHouse bootstrap and migrations, starts CP/Dispatcher, then
starts the Worker and waits for native `worker status`. It refuses to migrate
under a running application. There is no background retry controller or automatic
reboot startup. Startup failure returns nonzero and retains services and data for
inspection; it is not reported as an accepted environment. Read unit journals with
`journalctl -u helmr-verification-control-plane -u helmr-worker`, and the analogous
backing-service units. Do not export private configuration with logs.

`stop` first invokes native `worker drain --wait-timeout 5m`, then stops Worker,
Dispatcher, CP and backing services in that order. Failed drain or unexpected
Worker state stops the operation before dependencies are removed. Diagnose the
retained environment; do not interpret a timeout as permission to erase it.
Stopping retains all host/S3 data. It is neither fixture cleanup nor environment
destruction. Whole-scope cleanup belongs
to Cloud and must exist before unattended allocation. No host is allocated by
these commands.

## Stopped-host reuse

Stop the profile only after selected cases and their native fixture cleanup have
settled. The Cloud operator then stops the dedicated instance, retaining its disk,
installed profile, rooted tool closures, authentication/project and artifacts.
This is not an API for pausing a partly executed test. Each later case starts fresh.

On host restart verify KVM, configured NBD devices and private service files. Keep
the native Worker and profile units disabled for automatic boot startup, so `start`
owns dependency/migration order. Inspect installed identities before starting;
inactive services are expected while stopped. A schema mismatch, corrupt candidate
or pending update is not expected and must not be bypassed. Run `start` then require
`inspect` success. Native Worker re-enrollment after a completed drain may replace
its logical identity; do not infer that retained EC2 means the old Worker survived.
User authentication and data generation should survive ordinary stop/start; an
explicit schema reset still invalidates database-backed keys and requires setup.

The repeated-start behavior and warm latency require a real stop/start acceptance
check. Previous disposable-host Task/reset results do not establish this boundary.

## Resuming an existing scope

Run the existing `inspect` command on the already authorized host. It emits one
JSON observation containing service states, accepted CP/Dispatcher component
sources, expected/installed/running executable hashes, data generation and schema,
and any pending update's identity and bounded result phase. It reads
`installed-candidate.json`, `binary-digests.json`, `data-generation.json` and
`pending-update.json` under `/etc/helmr/verification`. It does not change them.

Exit 1 with `status: blocked` identifies missing/corrupt receipts, schema or byte
mismatches, inactive/unobservable services, or an incomplete update. A pending
marker remains blocking even when the attempt's result says `service-ready` or
`update-failed`; inspection never repairs or removes it. A missing result after an
interruption remains unknown. A pending update blocks further starts and updates.
Collect the attempt result and relevant journals, then destroy and recreate this
disposable environment through Cloud's native runbook. Do not remove the marker
to make a partially updated host appear healthy. There is no rollback or resume
command. Keep a healthy environment across ordinary failed behavior cases; a
failed test alone does not require recreation.

## First ordinary Task case

The `tests/e2e/cases/task` fixture contains just a sandbox and a Task that
round-trips a unique marker through the guest filesystem and returns run/computer
identities. It has no package-install layer, Actor dependency, combined smoke suite
or case registry. Add or remove ordinary `tests/e2e/cases/<behavior>/` files with
the behavior that needs them.

Prepare a separate, self-contained case project with local SDK tarballs:

```sh
nix develop -c bun install --frozen-lockfile
nix develop -c python3 tests/e2e/prepare_project.py /private/case-project --fixtures cases/task
```

The new directory contains the current editable cases/tasks, local packed SDK and
Proto dependencies, and its own lockfile. No npm publication is performed. BuildKit
receives all dependency files inside the project; repository-relative `file:`
dependencies and TypeScript configuration outside it cannot survive source capture.
Keep the prepared directory as the attempt's input; prepare another after changing
cases or SDK source. The source checkout's dependencies remain useful for local
type checks, but prepare an explicit fixture selection before deployment.

Use a CLI with an exact canonical bundle-builder image. A plain `go build` has no
builder selection and cannot build a deployment. For private source work, reuse a
verified published builder only when its Runtime descriptor and builder/compiler
inputs match the candidate; otherwise build the normal builder image from source.
Do not publish a release merely to prepare this private case.

After normal self-hosted setup has produced an environment API key, use the native
CLI to deploy this small project into that isolated scope. Deployment uploads and
builds are live operations and require the scope's authorization:

```sh
helmr deploy /private/case-project
HELMR_EVIDENCE_DIR=/private/attempt-001/task \
  bun run /private/case-project/cases/task/run.ts
```

When using the local Vite console with separate tunnels, point CLI/SDK
`HELMR_API_URL` at the forwarded CP port, not the Vite port. Vite proxies `/api/`
and `/dev/` for browser authentication but does not proxy the CLI/SDK's `/v1/`
routes. For example, a console on `127.0.0.1:58080` can use a CP tunnel on
`127.0.0.1:58081`; the latter is the CLI/SDK origin. OAuth still returns to the
console's configured callback.

Both commands use `HELMR_API_URL` and `HELMR_API_KEY`. The evidence directory must
not exist, and its parent must exist. The case uses bounded requests and execution
waits, fails on mismatched output, and writes `task.json` on execution/cleanup
failure as well as success. A Workspace delete request being accepted is recorded
as exactly that; it is not proof of physical cleanup or zero S3 storage. An
ambiguous create timeout retains its marker/idempotency key for diagnosis.

A passing case proves the ordinary API → dispatcher → Worker → guest → result
path for these assertions only. Persistence/resume and a CP-only candidate update use the separate paths below.
Provider behavior and complete scope teardown are not implied by either case.


## Candidate preparation and service updates

Use the pinned Nix toolchain and a clean, committed Product checkout. A private
local commit is sufficient: the output retains its source archive, so it is not
recoverable only from the original laptop. Keep output outside the checkout:

```sh
nix develop -c python3 dev/runtime/build_services.py . /private/candidate-002
sudo python3 dev/runtime/host.py apply-services --candidate /private/candidate-002
```

The builder hashes actual Go dependency/embedded-file inputs, schema source and
non-Go runtime/build inputs. It produces Linux AMD64 API-only CP and Dispatcher
binaries without rebuilding the Worker, guest or console. Candidates are trusted
operator build outputs, not signed third-party release artifacts. At initial
installation `services_candidate`, `worker_host_receipt` and
`worker_runtime_receipt` bind service source to the installed Worker manifests.
A later candidate is checked again from a private copy before touching services.

The default update restarts **only CP**. It requires unchanged schema, Dispatcher,
Worker, guest and runtime-support inputs and toolchain. It does not run migrations.
It compares retained service invocation IDs, PIDs and executable hashes before and
after, and verifies the actual new CP executable through `/proc`. Unchanged
Dispatcher bytes keep their original source identity. This does not prove that
an active guest survived a long CP outage; rerun the selected behavior cases, and
select an in-flight recovery case when that is the claim.

If CP startup, identity verification or receipt writing fails, the update remains
failed and its pending marker blocks further work. Collect the result and recreate
the environment. The updater does not retain rollback binaries or restore an old
candidate. Successful updates still reuse the current host and retained services.

## Edited migration files and disposable data reset

During unreleased development, editing an existing migration is supported.
Content hashes, not only the highest migration number, detect the change.
The default update rejects any schema change before stopping a service. To
explicitly discard this dedicated scope's fixtures and apply the new schema:

```sh
sudo python3 dev/runtime/host.py apply-services \
  --candidate /private/candidate-003 --reset-data
```

This updates CP **and Dispatcher**, after draining/stopping the old Worker against
the old database. It requires inactive service units, disconnected owned NBD
devices and no remaining mounts beneath the reset paths. It clears this profile's
PostgreSQL, Redis and ClickHouse state, Worker credentials/work state and jailer
state, then recreates the database cluster and runs the new native bootstrap and
migrations. Worker/guest binaries, runtime descriptor, installed guest images and
scope root keys remain. A new data-generation ID distinguishes observations before
and after reset. Normal self-hosted setup, project/environment/API keys and Task
deployment must be recreated; old fixture IDs and API keys are no longer valid.

Worker/guest/runtime input changes still require matching artifacts and a new
profile. `--reset-data` cannot bypass that check. Schema reset is intentionally
destructive to private fixtures and has no data rollback. A failure keeps its
pending marker, candidate and phase evidence for inspection. Keep the host until
that evidence is collected or the owning scope is explicitly destroyed.

S3 CAS/platform objects are **not deleted by data reset**. Old fixture objects can
remain orphaned until the scope's final storage cleanup; each reset is not a
zero-storage claim. This path never resets shared staging or production and does
not change the migration policy for already released persistent installations.

## Failed reset

A reset runs in the native `helmr-verification-reset.service` so initialization
and its child processes have one inspectable lifecycle. The caller waits; no
retry is scheduled. If the caller disappears, inspect that unit and its journal.
Collect the failed attempt and destroy/recreate the environment if reset did not
finish. There is no saved-generation resume protocol or data rollback.

Do not start another operation while the native unit is still running. For a stuck
operation, stop the exact unit under the existing scope authority before host
teardown. Native service credentials come from the private profile or host role;
caller environment variables are not forwarded. Source/tool paths must remain
available while the unit runs.

The focused Linux check `dev/runtime/check_reset_process_boundary.py`
qualifies child-process containment on an authorized host. It is not a resume
acceptance suite. The dedicated-host check passed on 2026-09-27: a stopped reset
unit terminated its child process, and duplicate operation admission was rejected.

After a successful reset, repeat normal authenticated setup, project/environment,
API keys and case deployment before testing. Services ready is not case success.

The same-candidate reset was exercised on 2026-09-27: services restarted with a
new data generation, normal authenticated setup/deployment was repeated, and the
Task case passed afterward. A separate check changed the existing initial
migration, verified normal update rejection before any service/data-generation
change, and applied it with explicit reset. The new schema marker and generation
were observed, the same protected endpoint changed from HTTP 200 to 401 for the
old API key, and normal GitHub setup, key issuance, deployment and Task execution
passed afterward. Reset took 60.810 seconds and the subsequent Task 89.510 seconds
in that single run, excluding manual setup and deployment time.

A CP-only executable change also passed: the changed HTTP response and executable
were observed while Dispatcher, Worker, backing-service identities and data
generation remained unchanged. Host-side update and assertions took 2.170 seconds;
the subsequent Task passed in 91.507 seconds with the retained API key. These
measurements exclude candidate build/transfer and are not latency guarantees.

## Persistence and resume case

Deploy the prepared Task project and run its case on the dedicated host with normal
API credentials and authorized noninteractive sudo for the read-only observer:

```sh
HELMR_EVIDENCE_DIR=/private/attempt-003/persistence \
  bun run /private/case-project/cases/persistence/run.ts
```

The Task writes a unique marker/symlink and emits a random in-memory nonce before
a managed Token wait. The driver waits for a ready checkpoint and evidence that
the original VM was closed and reclaimed, then completes the Token. It checks
file/symlink state, the unchanged nonce, attempt number 1, and a different ready
runtime restored from that exact checkpoint. Merely reporting `waiting`, a hot
resume, or rerunning the function from its entry is insufficient.

The Product-owned observer uses a read-only PostgreSQL query with bounded waits.
No provider runner imports database internals, no DB mutation triggers the resume,
and the normal API owns all Task/Token actions. This is same-host restore; it does
not prove host-loss isolation or cross-host compatibility. `persistence.json`
retains assertions and fixture-cleanup outcomes. Terminal Tokens are not cancelled
again. API deletion acknowledgement is not final host/S3 cleanup proof.

## Recoverable capture failure case

`cases/capture-abort` is a dedicated Linux/KVM qualification case. It requires
matching CP, Worker and guest artifacts and a fresh profile when those inputs
change. It has not yet passed live qualification. Installing it, privately
publishing its matching artifacts, and running the host require the applicable
environment authorization.

Build `./dev/runtime/capturefault` with the pinned Go toolchain for the host.
Run it only on the exclusive verification host, with its ordinary native S3
credentials and the exact scope CAS bucket:

```sh
capturefault --bucket SCOPE_CAS_BUCKET --region us-east-1 --hold 360s
```

At profile creation, set its CAS URI to
`s3://SCOPE_CAS_BUCKET?endpoint=http%3A%2F%2F127.0.0.1%3A58089` (retain any existing
CAS prefix). This loopback proxy forwards requests only to that bucket. It uses
the ordinary S3 protocol and native host authentication; it is not a file CAS or
a production fault hook. Keep its process available until profile shutdown.
Do not use it on a shared host. Restarting the proxy clears its one-shot state;
restart only between fresh cases after the previous Run and cleanup settle.

Prepare `cases/capture-abort` with `tests/e2e/prepare_project.py`, deploy through
the normal authenticated CLI, and run its `run.ts` with the same API/key/evidence
variables and `HELMR_RUNTIME_HOST_TOOL` as persistence. The case arms exactly one
memory-upload failure, waits until that request has been held for 310 seconds,
cancels one of two captured members, and completes the first Token while the source is sealed.
The proxy delays a non-retryable S3 response
for 360 seconds, longer than the five-minute guest grant. Both CP leases renew
until the cancellation so an early cancellation does not end the upload before
the intended injected response. The Task must continue
with its original random in-memory nonce, file and Run. A read-only database
observation requires both captured members, abort acknowledgment, the unchanged
source writer and no replacement Instance. The cancelled Run must reach its
cancelled outcome while the healthy member continues. Its active interval writes
an increasing shared-file counter before emitting each structured log. While the
proxy still holds the frozen upload, the driver reads the maximum delivered
counter and passes that baseline through the first Token. The healthy member
requires the file counter to remain exactly equal and a wait-finally marker to be
absent after abort and restore. Missing/buffered final telemetry can conservatively
fail the case. This sampled execution probe is not an exhaustive scheduler trace.
A second Token wait then produces a successful checkpoint;
the case requires source reclamation before completing that Token and verifies
restoration from the new checkpoint with the same memory/files/Run identity.

`HELMR_EVIDENCE_DIR/result.json` records the assertion results and normal fixture cleanup.
An interrupted proxy, expired request or unobserved failure is a failed case.
This case covers delayed upload failure, cancellation and resolution during
capture; it does not alone qualify lost guest/Control Plane replies.

### Lost capture-abort replies

For this separate exclusive-host case, create a fresh profile with
`"capture_reply_faults": true` and the same S3 fault endpoint above. Start
`capturefault --bucket SCOPE_CAS_BUCKET --region us-east-1 --hold 360s --replies` as root
before starting the profile. Only Worker uses the additional loopback relay at
58088; Dispatcher and user clients continue to use CP directly at 58080.
The runtime binaries are ordinary source-bound builds, with no fault switches.

Deploy `cases/control-plane-outage` and `cases/capture-abort` in the selected
fixture project. First run `cases/reply-relay/run.ts` with ordinary API credentials
and a fresh evidence directory. It interposes the original source vsock socket,
starts a real Computer Command through that relay, restores the original socket
name while a stream remains active, and requires the same Command/Instance to
finish with its marker. A failed passthrough preflight is not permission to arm
reply loss. Finish normal fixture reclamation, then restart only the proxy to
clear its one-case state.

Run the ordinary `cases/capture-abort/run.ts` with
`HELMR_CAPTURE_REPLY_LOSS=1`. During the existing upload hold it arms the relay
for that exact Computer, Instance, checkpoint and both sealed Run identities.
The relay consumes a complete successful upstream response before losing one CP
abort/grant reply, one guest prepare reply, one guest activation reply and one CP
completion reply, in that order. It forwards all other traffic. Evidence records
identity, timestamps, grants' expiry/disposition and whether a reply was dropped;
it never records authentication headers or write capabilities.

The case requires all four drops, real successful retries, current unexpired
grants, stable cancelled disposition, and a final acknowledged abort receipt.
All memory/file/Run identity, cancellation-counter and later restore assertions
still apply. An unreached drop, upstream failure or incomplete response is not
qualification. Unit checks for the tool and assertions are:

```sh
nix develop -c go test -race ./dev/runtime/capturefault
nix develop -c bun test tests/e2e/support/reply-fault.test.ts
```

The Unix socket listener is restored before forwarding the final acknowledged
abort reply. Existing relayed Program streams remain open until their normal
close, so keep the proxy running through fixture/source reclamation. The source
socket is renamed only under its exact dedicated jail path, with inode and
ownership checks; a changed or missing endpoint is a failed restoration, never
overwritten. After an interrupted case, inspect its receipts and request
`DELETE http://127.0.0.1:58089/__replies` before stopping the proxy. Complete ordinary
Run/Computer cleanup and confirm source reclamation before proxy/profile shutdown.
Do not rename sockets on a shared host or reuse an interrupted case.
There is no process-crash recovery: if the proxy dies while interposed, the source
socket name still points at the dead listener. Preserve the sibling original socket
and case evidence; use the normal operator cleanup path before recreating the
profile. Do not stop the proxy as a shortcut while a source still uses its streams.

## Actor Turn continuity

After deploying the same small project with normal credentials, run on the host:

```sh
HELMR_EVIDENCE_DIR=/private/attempt-004/actor \
  bun run /private/case-project/cases/actor/run.ts
```

The Actor creates a random nonce in memory once, writes its marker to a private
file, and waits on a Token during the first Turn. The driver observes the nonce
before completion and waits for a ready checkpoint plus the original VM's closure
and reclamation. It then completes the Token, checks the first Turn's result and
submits a second Turn. The nonce, file, Run/Session/Workspace identity and in-memory
counter must persist; the counter must advance from one to two. Run retries are
disabled. Closing the Session must end its original Run successfully. The Product
observer checks Actor identity, attempt one, the exact previously observed
checkpoint and a different ready restored VM.

This is Actor-specific same-host continuation, not host-loss/cross-host acceptance
or proof of every Session/Turn behavior. It reuses the read-only Product query used
by the Task persistence case; Cloud receives no DB access. `actor.json` retains
assertion results, identities and separate cancellation, closure and deletion
outcomes. Cancellation and closure polling share a 180-second deadline; Workspace
deletion requires confirmed Session closure. A failed Session or unconfirmed
closure retains the Workspace for diagnosis instead of claiming cleanup. Creation
ambiguity, observation failures, terminal Turn failures and cleanup failures exit
nonzero. No accepted API deletion is promoted to final resource cleanup.

Offline assertion-sensitivity checks use
`nix develop -c bun test tests/e2e/cases/actor/assertions.test.ts`; they reject lost
memory, reset counters, changed identity, retried Runs, wrong checkpoints and hot
VM reuse. Type checking and the real PostgreSQL query check validate source/query
boundaries only. The complete Actor case still needs the integrated checkpoint.

## Guest IPv4 metadata case

Run `cases/network/run.ts` on the dedicated host with the same API/key/evidence
environment as persistence. It first requires successful public HTTPS from the
guest, then takes a native policy/counter baseline on that Run's exact retained
VM. A request to the IPv4 metadata index must return no HTTP response; the
metadata address must be in the installed deny set and the same namespace's
`run_denied` counter must increase. Token waits keep that VM alive for observation.
No credential path or IMDS token is requested. The counter is shared by several
deny rules, so this is not destination-specific tracing, IPv6 coverage or general
network-isolation qualification. This case passed on 2026-09-27: public HTTPS
returned 200, the metadata probe received no HTTP response, and the retained VM's
denied-packet count increased from 0 to 6. The case and fixture cleanup requests
completed in 115.746 seconds in that run.

Log replay can briefly return `telemetry_lagging`. The driver records and waits
through only that condition within the existing phase deadline; other errors or
a terminal Run still fail. The first live attempt exposed this condition and was
cancelled with fixture cleanup before the corrected case passed.

## Named database observations

`observe.py OBSERVATION INPUTS_JSON` renders one fixed, read-only observation as
psql input. The available observations cover Run placement/path and reclamation,
Computer state, current Worker Hosts, regional Worker capacity/platforms,
Deployment identity and environment API-key scope. Inputs contain exactly the
fields declared in `observe.py`; SQL text, file names and query fragments are not
accepted. Results are one JSON value. `run-path` includes exact checkpoint member
identities, source reclamation, restored lease lineage and artifact byte sizes.

Execute with the same Product source revision that created the database. When
an initial schema changes, reset the disposable database through the host or
deployment owner's normal greenfield reset before using these observations.
A matching migration version alone does not establish that identity. The
execution owner supplies authorized connectivity and enforces result bounds;
the renderer enforces a read-only transaction and short statement/lock limits.
No observation changes Product state or replaces native mutation APIs.

The populated PostgreSQL regressions run with
`nix run .#ci-postgres -- '^TestVerificationObservation' ./dev/runtime`.
External Capacity consumers can run a separately compiled executable against the
real HTTP router and disposable database through `TestCapacityExternalConsumer`;
set `HELMR_CAPACITY_CONSUMER_TEST` to its absolute path. The fixture passes its
endpoint and test-only credentials through the child process environment.


`host.py observe --observation NAME --inputs JSON` runs the fixed observation
against this profile's local database. `computer-path` includes Computer Instances,
checkpoint/disk lineage and Commands for planned-drain qualification. See the
[ordinary drain/outage cases](../../tests/e2e/README.md#dedicated-host-drain-and-observation-outages)
for their operator coordination and cleanup. The idle-Host
`check_fencing_outage.py` is destructive fault injection within an authorized
exclusive dev scope; it must not run against shared infrastructure.
