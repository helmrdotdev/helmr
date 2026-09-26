# Dedicated runtime host

This is a source-prepared feasibility profile, not a qualified deployment target.
It composes the normal Control Plane, Dispatcher, PostgreSQL 18, Redis,
ClickHouse and installed Worker on one dedicated Linux x86_64 KVM/systemd host.
A passing `start` means CP and native Worker readiness, Redis PING and active
service processes only. The Task case
below provides a separate execution assertion. Live integrated acceptance is
still outstanding.

## Inputs and ownership

One entire host belongs to one repair scope. Reserve sufficient disk and memory
for the real services, verifier and guest; the Worker defaults include 64 GiB of
Computer staging. A scope spans multiple correction attempts. Do not install this
profile on shared staging or an existing development host.

Provisioning must supply:

- KVM, cgroup v2, systemd with `DelegateSubgroup`, Python 3.12+, `runuser`, the
  shared Worker's OS packages and explicitly assigned, disconnected NBD devices.
  Load/configure NBD during provisioning; this profile never borrows live devices
  or repartitions a disk. Choose network pools, DNS and blocked destinations for
  the actual host network, including metadata and other privileged endpoints.
- The source-owned shared Worker installer and digest-verified host/runtime
  bundles described in [README](README.md). Run its `install` before installing
  this profile. Its systemd unit remains the Worker execution boundary.
- Real, isolated S3 CAS and platform storage with authorized native AWS credentials
  or a host role. The normal runtime release must already be privately published,
  with its actual descriptor. No file CAS, synthetic descriptor or public release
  is part of this path. This script does not grant AWS access or publish artifacts.
- A `build-services.py` candidate directory containing Linux CP/Dispatcher binaries,
  input digests and the clean archived source, plus the native Worker host/runtime
  bundle receipts from that initial source revision. These API-only service binaries
  do not embed the console; normal setup/API authentication is still required.
  Supply absolute paths to the
  backing services from the pinned toolchain. Provisioning must retain their Nix
  closure/profile if using Nix; a development shell alone is not a GC root.
- Normal OAuth configuration and a normal Worker enrollment token. Normal
  self-hosted setup and environment-scoped API keys are still required for cases;
  no seeded user or authentication bypass is installed.

Copy [runtime-host.example.json](runtime-host.example.json) to a private file and
replace its placeholders. `control_plane` and `worker` contain ordinary native
configuration, including their own AWS settings where needed. The script owns
local endpoints, default region/group/pool, store agreement, key generation and
service identities, and rejects overrides of those fields. Per-scope keys and
passwords are generated once on install and retained across starts. Secrets never
belong in source control or the evidence bundle.

The profile uses self-hosted Computer key wrapping. Backing services and CP bind
loopback; access CP through an authorized tunnel. PostgreSQL uses a separate
non-superuser application role with SCRAM authentication over loopback, and
ClickHouse uses the real bootstrap command to create separate reader, ingester
and migration credentials. This does not prove managed TLS/IAM/KMS, provider
scaling, rollout behavior or cross-host restore.

## Commands

Offline rendering validates configuration shape and writes a new private directory:

```sh
python3 dev/verification/runtime-host.py render \
  --config /private/runtime-host.json --output /private/runtime-host-render
```

After the provisioning and live-use authorization applicable to the scope:

```sh
sudo python3 dev/verification/runtime-host.py install --config /private/runtime-host.json
sudo python3 dev/verification/runtime-host.py start
sudo python3 dev/verification/runtime-host.py inspect
sudo python3 dev/verification/runtime-host.py stop
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

`stop` first invokes native `worker drain --timeout 5m`, then stops Worker,
Dispatcher, CP and backing services in that order. Failed drain or unexpected
Worker state stops the operation before dependencies are removed. Diagnose the
retained environment; do not interpret a timeout as permission to erase it.
Stopping retains all host/S3 data. It is neither fixture cleanup nor environment
destruction. Independent expiry, orphan recovery and whole-scope cleanup belong
to Cloud and must exist before unattended allocation. No host is allocated by
these commands.

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
`rolled-back`; inspection never repairs or removes it. A missing result after an
interruption remains unknown. Use the bounded CP recovery below when applicable before performing another
update or case. For an interrupted destructive reset use `resume-reset` below, not CP rollback.

Exit 0 with `status: observed` means only that these checks found no blocker. It
is not case acceptance, Worker/guest artifact verification, an atomic snapshot,
or permission to resume mutations. Compare the report with the intended
recoverable candidate and original Worker receipts. Scope ownership and expiry
still belong to Cloud's capsule. Secret configuration, environment files, keys,
full attempt errors and database credentials are excluded from this output.
Resume uses the existing scope; it does not install this profile again.

## Recovering an interrupted CP-only update

Within the existing scope's live-operation authorization, run:

```sh
sudo python3 dev/verification/runtime-host.py recover-services
```

This always restores the **previous CP**, never silently finishes the target
update. Before publishing a pending update, the updater now retains the old CP
and previous candidate, digest and data-generation receipts in its owned attempt.
Recovery requires those complete records, a matching target archive, unchanged
data generation/schema and retained service processes, matching Dispatcher bytes,
and installed CP bytes belonging to either the old or target candidate. A host
reboot, unrelated replacement or incomplete/corrupt evidence requires diagnosis;
the command does not guess what was previously installed.

Recovery stops/replaces/starts only CP, checks readiness and actual executable
identity, restores the previous metadata and writes a new recovery result before
removing the pending marker. Original attempt evidence stays intact. A failure or
process interruption retains the marker; another recovery invocation reconciles
the same attempt and creates separate evidence. This covers process interruption,
not a qualified host power-loss or filesystem durability guarantee. An update
interrupted before all rollback inputs were saved cannot be recovered by inventing
those inputs from the current candidate.

An interrupted data reset is rejected by `recover-services`: already erased
fixtures cannot be rolled back by replacing a binary. Use the explicit reset
retry below when completing that same destructive operation is authorized. Recovery does not
allocate, reset data, change Worker/guest artifacts, close the scope or extend its
expiry. A successful CP recovery means the previous candidate is restored; the
failed target update remains failed and selected behavior cases must run again.

## First ordinary Task case

The small project in this directory contains just a sandbox and a Task that
round-trips a unique marker through the guest filesystem and returns run/workspace
identities. It has no package-install layer, Actor dependency, combined smoke suite
or case registry. Add or remove ordinary `tasks/*.ts` and `cases/*.ts` files with
the behavior that needs them.

Prepare the local SDK packages and case dependencies:

```sh
nix develop -c bash -c 'bun install --frozen-lockfile && scripts/build-npm-packages.sh'
nix develop -c bun install --cwd dev/verification --frozen-lockfile
```

After normal self-hosted setup has produced an environment API key, use the native
CLI to deploy this small project into that isolated scope. Deployment uploads and
builds are live operations and require the scope's authorization:

```sh
helmr deploy dev/verification
HELMR_EVIDENCE_DIR=/private/attempt-001/task \
  bun run dev/verification/cases/task.ts
```

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
nix develop -c python3 dev/verification/build-services.py . /private/candidate-002
sudo python3 dev/verification/runtime-host.py apply-services --candidate /private/candidate-002
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

If CP startup or identity verification fails, the updater attempts to restore
only the previous CP binary and its candidate metadata. The failed attempt remains
recorded even when rollback succeeds. If interrupted, or recovery fails, the
pending marker blocks further updates/ordinary starts. Inspect that attempt's
`result.json`, saved previous CP and journals; do not delete the marker to make
an inconsistent environment appear ready. Use `recover-services` for a qualifying CP-only interruption. Data resets use `resume-reset`; changed retained processes remain outside the
CP rollback contract.

## Edited migration files and disposable data reset

During unreleased development, editing an existing migration is supported.
Content hashes, not only the highest migration number, detect the change.
The default update rejects any schema change before stopping a service. To
explicitly discard this dedicated scope's fixtures and apply the new schema:

```sh
sudo python3 dev/verification/runtime-host.py apply-services \
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

## Completing an interrupted data reset

When the same disposable-data deletion is authorized, run on the existing host:

```sh
sudo python3 dev/verification/runtime-host.py resume-reset
```

Both initial reset and retry execute inside the fixed native
`helmr-verification-reset.service`. The caller waits for its result; it does not
run an initializer outside this boundary. `ExitType=cgroup` retains the service
while any child remains, and `KillMode=control-group` stops its descendants. A
second launch under the same unit name is rejected while the first remains;
there is no direct-execution fallback. These properties follow the upstream
[service lifecycle](https://raw.githubusercontent.com/systemd/systemd/main/man/systemd.service.xml)
and [process termination](https://raw.githubusercontent.com/systemd/systemd/main/man/systemd.kill.xml)
contracts. This host profile already requires recent systemd.

After caller loss, inspect `systemctl status helmr-verification-reset.service` and
its journal before retrying. If terminating a stuck operation is within the scope's
authorization, stop that exact service and wait for it to stop; never kill only the
Python PID or clear a marker. Native service credentials must come from the
profile's private configuration or host-native role/credential files, not ephemeral
caller environment variables, which are not forwarded. The private source/tool
paths must remain present while the service runs.

At the authorized Linux checkpoint, run
`python3 tests/runtime_host/check_reset_process_boundary.py` as root to test the
production supervision properties with a harmless surviving initializer process
through the same native `runuser`/`helmr-services` identity as initialization. The
profile service account and readable source/tool paths must already exist.
It kills the main test process, checks duplicate-unit rejection, stops the group,
and checks the child is terminal; it retains a private evidence directory. It
creates only a uniquely named test unit and does not reset Product data. This
native check has not run on the workstation; passing unit tests alone do not
qualify the process boundary.

This repeats initialization for the **saved pending candidate**. It accepts no new
candidate and cannot roll a reset back. Both the first reset and this retry use
one implementation: quiesce, discard private fixtures, initialize storage, install
CP/Dispatcher, perform native bootstrap/migrations/enrollment and verify process
identity. It restarts from clean private data instead of guessing which migration
or initialization statement last completed.

The original reset fixes a new data-generation ID. That ID is published before
fixture deletion and is retained across retries of this logical reset. Each retry
writes a new result alongside the original attempt evidence. An unrelated data
generation, different candidate/runtime inputs, unknown binary, corrupt receipt,
or missing required quiescence record blocks before deletion. No replacement
scope or new lifetime is created. Old API keys and fixture IDs remain invalid.

The old Worker must complete normal drain before the first erase; a durable record
in the owned attempt marks that boundary. On a later retry, a failed partially
started Worker may be stopped, but all services must then be inactive and the
existing NBD/mount checks must pass before erasure. An active Worker's failed drain
never falls back to forced termination. Pending markers survive failures and
interruptions; do not delete them manually. This is process-interruption handling,
not a qualified host power-loss guarantee.

Success reports `services-ready-fixtures-required` and removes the pending update
only after process and receipt verification. **Repeat normal self-hosted setup,
project/environment/API-key creation and case deployment** before running selected
cases. No user is seeded, credentials bypassed or previous test success reused.
These authenticated steps are not automated by the host reset command. S3 cleanup,
Worker/guest artifact replacement and whole-scope closure remain separate.

## Persistence and resume case

Deploy this directory's Task project and run on the dedicated host with normal
API credentials and authorized noninteractive sudo for the read-only observer:

```sh
HELMR_EVIDENCE_DIR=/private/attempt-003/persistence \
  bun run dev/verification/cases/persistence.ts
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

## Actor Turn continuity

After deploying the same small project with normal credentials, run on the host:

```sh
HELMR_EVIDENCE_DIR=/private/attempt-004/actor \
  bun run dev/verification/cases/actor.ts
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
`nix develop -c bun test tests/runtime_actor/actor-check.test.ts`; they reject lost
memory, reset counters, changed identity, retried Runs, wrong checkpoints and hot
VM reuse. Type checking and the real PostgreSQL query check validate source/query
boundaries only. The complete Actor case still needs the integrated checkpoint.
