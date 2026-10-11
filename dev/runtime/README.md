# Dedicated runtime host

This is a dedicated development verification profile, not a general deployment target.
It composes the normal Control Plane, Dispatcher, PostgreSQL 18, Redis,
ClickHouse and installed Worker on one dedicated Linux x86_64 KVM/systemd host.
A passing `start` means CP and native Worker readiness, Redis PING and active
service processes only. Run the selected Agent cases against the exact candidate
to verify execution, persistence, service changes, network isolation or recovery.

## Inputs and ownership

One dedicated host serves one repair objective at a time. It may be stopped and
retained between objectives. Reserve sufficient disk and memory
for the real services, verifier and guest. The sample caps Computer staging at
4 GiB for these small cases; the native default is 64 GiB. Reserve space for
the live disks and checkpoint intermediates together; capacity sufficient for
execution may still be insufficient for checkpoint capture. A scope spans
multiple correction attempts. Do not install this
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
  bundle receipts from that initial source revision. Service binaries are API-only
  by default; use the builder's `--console` option when the verification requires
  the production Console. Normal setup/API authentication is still required.
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

The sample includes explicit development execution and diagnostic budgets. Adjust
them for the selected host. Diagnostic admission and export settings reach both
Control Plane and Dispatcher; Worker log buffers remain Worker inputs. Native
service configuration checks run before dependencies or migrations start.

Configure each Agent's dedicated Slack App through Console after normal setup
and promotion of the Agent's deployment. Use that registration's generated
manifest and store its App credentials through the setup form. App credentials and installation tokens are
held by the Control Plane; no global `SLACK_*` settings belong in this profile.

For an explicitly authorized external HTTPS route, set the optional top-level
`public_url` to its origin, such as `https://verification.example.test`. It must
have no credentials, path, query or fragment. Control Plane and Dispatcher use
this origin for normal authentication callbacks and public links. Their internal
transport and all backing-service listeners remain on loopback. Omitting the field
preserves the loopback public URL. This setting does not create a tunnel, DNS record
or certificate, open a port, configure an OAuth app or authorize public exposure.
Complete normal setup under the chosen operator-only access restriction before
enabling public callbacks; do not expose a development login or Vite server.
The public origin is fixed at profile installation. Changing it requires a new
profile and normal setup; neither `apply-services` nor a data-only reset changes
it. Do not hand-edit the saved profile or environment files. An external TLS
terminator must set `X-Forwarded-Proto: https` and replace untrusted client
`X-Forwarded-For` values; qualify Secure cookies through the actual route.


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

## First ordinary Agent case

The `tests/e2e/cases/agent` fixture contains a Computer definition and an Agent
whose initial Turn round-trips a unique marker through the guest filesystem and
returns Turn, Session and Computer identities. The driver waits for the Turn
outcome, cancels the Session, and requests Computer deletion. Add or remove ordinary `tests/e2e/cases/<behavior>/` files with
the behavior that needs them.

Prepare a separate, self-contained case project with local SDK tarballs:

```sh
nix develop -c bun install --frozen-lockfile
nix develop -c python3 tests/e2e/prepare_project.py /private/case-project --fixtures cases/agent
```

The new directory contains the current editable cases/agents, local packed SDK and
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
  bun run /private/case-project/cases/agent/run.ts
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
non-Go runtime/build inputs. It produces Linux AMD64 CP and Dispatcher binaries
without rebuilding the Worker or guest. By default, it omits Console assets. With
`--console`, it installs frozen JavaScript dependencies without lifecycle scripts
inside the private source archive, runs the ordinary `console-build`, and applies
`embed_console` to both Go dependency enumeration and compilation. The receipt
therefore hashes the actual embedded assets. Candidates are trusted
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

To replace CP and Dispatcher together without discarding data, add
`--include-dispatcher`. Schema, Worker, guest, runtime-support and toolchain inputs
must still match. The updater stops Dispatcher before CP, replaces both binaries,
checks both candidate environments before stopping services, waits for CP readiness,
then starts Dispatcher and waits for its existing startup-complete log in the exact
systemd invocation. It verifies both running executable
hashes and preserves the data-generation receipt and all backing/Worker processes.
This option cannot be combined with `--reset-data`; it performs no migrations.
Start a stopped profile with its installed services before applying the candidate.

If service startup, identity verification or receipt writing fails, the update remains
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
and after reset. Normal self-hosted setup, project/environment/API keys and Agent
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

## Replace an incompatible disposable profile

When Worker/runtime inputs or the installed public origin must change, a
service-only update is insufficient. With explicit authority to replace this
profile, use `retire-profile` to preserve its private configuration and data on
the same disk before the normal shared Worker installer and a fresh installation:

```sh
sudo python3 dev/runtime/host.py retire-profile \
  --expected-source EXACT_INSTALLED_COMMIT \
  --expected-generation EXACT_DATA_GENERATION_UUID
```

First resolve the exact host/scope, original candidate and data generation, prior
fixture cleanup, and any external installation tied to the old database. Preserve
the installed Worker/runtime bundles and digest receipts before overwriting their
shared paths. Record disk capacity, retained private data and any leftover Worker
network resources; do not treat service inactivity as completed fixture cleanup.
Unresolved external work blocks replacement. This command does not clean S3.

The command requires all profile services and Worker inactive and disabled, no
remaining service/guest processes, disconnected owned NBD devices, unmounted data,
and exact saved service definitions with no foreign Worker overrides. It refuses
an unfinished update or retirement. It moves owned paths by same-filesystem renames
into a new root-only `/var/lib/helmr-retired/<data-generation>/` directory. The
archive contains credentials and private data and stays under the existing
encrypted-disk custody. Shared runtime images, installed binaries, tool closures
and artifact caches remain unchanged. Budget space for archived and new data.

An absent Worker work directory is recorded and skipped because the Worker creates
it lazily and initial startup may fail before it runs. Other owned paths remain
required; symlink, mount, process and ownership guards still apply. Capture failure
evidence and clear failed unit state explicitly before retirement.

The first profile mutation renames `config.json` to `retired-config.json`, making
the active configuration unavailable even to an older installed script. An
interrupted retirement keeps that directory, blocking installation and further
profile changes; a pending record identifies the archive if its write completed.
Inspect the archive and active
paths; do not remove the marker to bypass the failure. There is no automatic retry,
resume or rollback. Success moves configuration last and reports the archive path.
Install the exact current verified Worker/runtime bundles next, then run ordinary
`install`, setup and enrollment with fresh keys/database. This is not a continuity
test or permission to purge the archive; archive removal needs its own disposition.

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
acceptance suite. It checks that stopping a reset unit terminates its child
process and that duplicate operation admission is rejected.

After a successful reset, repeat normal authenticated setup, project/environment,
API keys and case deployment before testing. Services ready is not case success.

## Persistence and resume case

Deploy the prepared Agent project and run its case on the dedicated host with normal
API credentials and authorized noninteractive sudo for the read-only observer:

```sh
HELMR_EVIDENCE_DIR=/private/attempt-003/persistence \
  bun run /private/case-project/cases/persistence/run.ts
```

The first Turn writes a marker/symlink and returns a nonce retained in Session
setup memory. After that Turn completes, the driver requires an idle ready
checkpoint and physical source reclamation. It enqueues a second Turn, verifies
the same nonce/files and Session identity, and observes the exact restored
allocation while the second Turn waits on a `"finish"` message. Only after those
observations does the driver allow that Turn to complete. Questions and authored
timers keep Turns active; they are not checkpoint eligibility signals.

The Product-owned observer uses bounded read-only PostgreSQL queries. Native APIs
own Session admission, answers and cleanup. This is same-host restore; it does not
prove cross-host compatibility. `result.json` and separate persistence observations
retain assertions and cleanup outcomes. API deletion acknowledgement does not prove
final host/S3 reclamation.

## Recoverable capture failure and lost replies

`cases/capture-abort` is a dedicated Linux/KVM qualification case. Matching CP,
Worker, guest and fixture artifacts are required. The migrated case still needs
live qualification. Publishing artifacts, starting a host and executing faults
require the applicable environment authorization.

Create an exclusive profile with `"capture_reply_faults": true`. Build
`./dev/runtime/capturefault` with the pinned Go toolchain and start it as root before
the profile, using the ordinary host authentication and exact scope CAS bucket:

```sh
capturefault --bucket SCOPE_CAS_BUCKET --region us-east-1 --replies
```

Only Worker uses the additional CP relay at loopback port 58088; Dispatcher and
user clients use CP directly at 58080. Keep the normal CAS URI. This case does not
arm the tool's separate S3 upload-failure injector: publication retry is a different
boundary from a source abort before disk capture. Runtime binaries contain no fault
switches. The proxy is one-case state; restart it only after fixture reclamation.

Prepare `cases/control-plane-outage` and `cases/capture-abort` in the fixture project.
First run `cases/reply-relay/run.ts` with normal API/key/evidence variables on the
host. It interposes the exact source vsock socket, starts a Computer Command through
it, restores the socket pathname with a stream active, and requires the same Command
and Instance to finish with its marker. After successful preflight and ordinary
fixture reclamation, restart the proxy to clear its state.

Run `cases/capture-abort/run.ts` with `HELMR_RUNTIME_HOST_TOOL` and a fresh evidence
directory. Both Sessions remain active until the driver arms the relay for their
exact Computer, source Instance, writer generation and process epochs. After both first
Turns complete, the relay reads the complete frozen Capture receipt and closes the
reply without forwarding it. It gates every matching Inspect retry until the
driver releases the gate; other streams, including authority renewals, continue.
The gate has a three-minute failure bound.

The driver waits beyond the actual capture envelope deadline and verifies that
current CP authority remains live. While the source is frozen, it enqueues a Turn
on the Session selected for cancellation, cancels that Session, and enqueues the
healthy Session's second Turn. All admissions and cancellation must finish before
the driver releases Inspect. The relay then loses one successful CP preparation
reply, guest Install reply, guest Activate reply and CP completion reply. Install's
bidirectional authority-clock exchange is forwarded unchanged. Incomplete or failed
responses never count as applied-but-lost evidence.

Before installation, refreshed authority can replace an expired preparation. Once
installed, every retry must retain the full exact installation. The relay records
safe identities/digests and current stopped-session controls, never credentials or
serialized authority. The case requires all drops, successful retries and listener
restoration before the final CP completion response reaches Worker.

The healthy member must retain setup memory, files and Session identity. Each
Turn has its own identity. The cancelled queued Turn must stay cancelled without
entering its handler: it would increment a file counter and write an execution
marker immediately on entry. Both must remain unchanged/absent after source abort
and later restore. Current guest controls and activation receipts also require the
captured cancelled process to stop before live peers activate.

The old active-question/interval/finally probe is retired: a running question or
unqualified authored timer cannot enter coherent capture. The new counter checks
queued handler entry; it does not claim to trace arbitrary idle process execution.
The driver keeps the healthy second Turn active while observing the exact aborted
checkpoint, then permits completion. A new idle checkpoint must become durable and
reclaim its source before the third Turn runs on the restored allocation. The
aborted checkpoint cannot serve as that restored checkpoint.

```sh
nix develop -c go test -race ./dev/runtime/capturefault
nix develop -c bun test ./tests/e2e/support/reply-fault.test.ts
```

Socket restoration preserves accepted streams until they close naturally. The
relay checks original/proxy socket inodes and ownership; a changed pathname is never
overwritten. On interruption request `DELETE http://127.0.0.1:58089/__replies` to
release the gate and restore the listener, then finish native Session/Computer
cleanup and source reclamation before stopping the proxy. A dead proxy has no
process-crash recovery: preserve its sibling original socket and evidence and use
normal operator cleanup before recreating the profile. Do not use this on shared
hosts or stop a proxy while the source still uses its streams.

## Session Turn continuity

After deploying the selected project, run on the dedicated host:

```sh
HELMR_EVIDENCE_DIR=/private/attempt-004/sessions \
  bun run /private/case-project/cases/sessions/run.ts
```

The Agent's setup creates one random nonce and an in-memory counter. The first
Turn writes a marker and completes. The driver requires a ready idle checkpoint
and source reclamation before enqueuing the second Turn. Nonce, file and
Session/Computer identities persist; the counter advances from one to two. The
second Turn emits its state and waits for a `"finish"` message while the observer
checks the exact checkpoint and a different current runtime. Cleanup cancels the
Session and deletes the owned Computer.

This same-host claim does not prove host-loss or cross-host continuation. Local
assertion checks reject changed memory, counters, identities, checkpoint lineage,
unfenced sources and stale targets:

```sh
nix develop -c bun test ./tests/e2e/cases/sessions/assertions.test.ts
```

## Guest IPv4 metadata case

Run `cases/network/run.ts` on the dedicated host with the same API/key/evidence
environment as persistence. It first requires successful public HTTPS from the
guest, then takes a native policy/counter baseline on that Session's exact retained
VM. A request to the IPv4 metadata index must return no HTTP response; the
metadata address must be in the installed deny set and the same namespace's
`run_denied` counter must increase. Ordinary message gates keep the Turn active for host observation.
No credential path or IMDS token is requested. The counter is shared by several
deny rules, so this is not destination-specific tracing, IPv6 coverage or general
network-isolation qualification. Qualify the case against the exact candidate. Session
output pagination and retention are checked explicitly; terminal Turns or missing
phase evidence fail the case.

## Named database observations

`observe.py OBSERVATION INPUTS_JSON` renders one fixed, read-only observation as
psql input. The available observations cover Session placement/path and Computer lease fencing,
Computer state, current Worker Hosts, regional Worker capacity/platforms,
Deployment identity and environment API-key scope. Inputs contain exactly the
fields declared in `observe.py`; SQL text, file names and query fragments are not
accepted. Results are one JSON value. `session-path` reports only actual checkpoint
membership. `computer-path` reports lease fencing, checkpoint source/target epochs,
exact members, artifact byte sizes, disk saves and commands. These are recorded
facts; checkpoint membership alone does not prove a completed restore.

Execute with the same Product source revision that created the database. When
an initial schema changes, reset the disposable database through the host or
deployment owner's normal greenfield reset before using these observations.
A matching migration version alone does not establish that identity. The
execution owner supplies authorized connectivity and enforces result bounds;
the renderer enforces a read-only transaction and short statement/lock limits.
`slack-delivery` takes an exact Session ID and reports its thread status lane,
installation authorization/refresh state and newest 100 post delivery records.
It excludes credentials, message bodies and frozen request payloads.
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
