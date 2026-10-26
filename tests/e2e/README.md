# Product verification

Choose the smallest boundary that proves the change. A case is an ordinary source
file; adding or deleting a case does not require a registry or an all-suite gate.

| Boundary | Owner and entry |
| --- | --- |
| Logic, API/DB transitions | Native Go/TypeScript tests; focused commands below |
| Local API and browser work | `dev/local/start.sh`, `dev/local/browser.sh`, `dev/local/reset.sh` |
| Real Worker/guest behavior | Cases here, on the dedicated Dev host |
| Dedicated host service lifecycle and schema reset | [`dev/runtime`](../../dev/runtime/README.md) |
| Managed delivery, IAM, fleet and cross-host behavior | Deployment repository's managed validation |

Local, dedicated Dev and shared staging are execution destinations, not three
copies of each case. Use staging for an integrated rollout claim; ordinary feature
corrections do not require publishing a preview or rebuilding a managed stack.

See [test ownership and naming](../README.md) for repository-wide placement and
file conventions. `scripts/check-e2e.sh` checks types and local helper tests; it
does not execute real workloads.

## Layout and selection

- `cases/<behavior>/run.ts` and `*.run.ts`: executable stimulus, assertions and fixture cleanup.
- `cases/<behavior>/agent.ts`: guest Agent/Template definitions needed by that
  behavior. Related cases may use an existing fixture with a relative import.
- `support/`: bounded waits and evidence/cleanup mechanics shared by actual callers.
- `fixtures/schedule/`: intentionally separate scheduled deployment. Promote it
  only for trigger validation; restore the ordinary bundle afterward.
- `prepare_project.py`: isolate selected fixtures and locally packed SDK dependencies.

Keep assertions next to their stimulus. Split by a separately meaningful claim,
not one file per API call. Do not put deployment, account setup or provider fault
injection into an ordinary behavior case. External agent examples live in
`examples/issue-fixer`, outside the basic runtime project's dependencies.

| Cases | Fixture directories passed to preparation | Claim |
| --- | --- | --- |
| `setup-failure` (human-operated) | `cases/setup-failure cases/slack-conversation` | Initialization failure retains failure and physical Stop, then frees a one-slot host for a healthy new Computer |
| `agent` | `cases/agent` | Initial Turn marker and guest filesystem |
| `scoped-permissions` | `cases/scoped-permissions` | Two restricted key origins, native Computer/file separation, own continuity and cross-origin denial; dedicated scope and three VM slots |
| `persistence` (including `shared.run.ts`) | `cases/persistence` | Single-member and shared-Computer checkpoint and same-host restore |
| `execution-lifecycle` | `cases/execution-lifecycle` | Manual post-Turn JavaScript/native background progress, explicit Hibernate/Wake, stable initialization/process identity, and Wake without a Turn |
| `sessions` | `cases/sessions` | Two Turns retain Session setup memory across same-host restore |
| `network` | `cases/network` | Guest metadata denial plus exact host packet observation |
| `runtime`, `computer-command` | `cases/runtime` | Runtime tools/files/output and Computer idempotency/exec |
| `runtime/program-replacement.run.ts` | `cases/runtime` | Existing Session pins its Deployment; new Session uses promoted code on the same Computer |
| `prepared-startup` | `cases/prepared-startup` | Warm adoption versus cold/zero policy, refill, fresh secrets, no preclaim Agent init and private writes; dedicated host with exact resource/Environment IDs |
| `computer-command/seed-reuse.run.ts` | `cases/runtime` | Shared seed root, independent writes, deletion isolation and create-to-first-work timing; dedicated host and two VM slots |
| `questions`, `question-isolation`, `question-cancel` | `cases/questions` | Pending questions, repeat response receipts, isolation between Sessions and cancellation |
| `slack-conversation` (human-operated) | `cases/slack-conversation` | Synthetic progress, questions, multiple speakers and settlement for [native Slack observation](cases/slack-conversation/README.md) |
| `session-continuity`, `helpers` (including `cancel-peer.run.ts`) | `cases/helpers` | Spawned and independent helpers, shared-Computer peers, Session memory and paginated durable output |
| `planned-drain` | `cases/planned-drain cases/control-plane-outage` | Warm Computer capture with queued Command and fresh logical Host restore |
| `control-plane-outage` | `cases/control-plane-outage` | Live Turn survives CP outage beyond the stale-Host window with Dispatcher continuously active |
| `drain-renewal` | `cases/drain-renewal` | Resident Turn retains its lease through more than 30 minutes of planned drain |
| `delay`, `session-cancel` | `cases/delay` | Ordinary JS delay completion and cancellation of active/queued Turns |
| `execution-policies` | `cases/execution-policies` | Ordinary/finite/blocked networking, distinct preparation policies, actual Computer pin on reuse, scoped MCP/receipt denials, and retained descriptors after explicit Hibernate/Wake |
| `network-egress` | `cases/network-egress` | Public IPv4 succeeds, no IPv6 default route |
| `computer-overwrite`, `concurrent-questions`, `invalid-payload`, `expected-error` | `cases/computer-overwrite` | Filesystem overwrite, independent concurrent questions and handler failure contracts |
| `secret-injection`, `missing-secret` | `cases/secret-injection` | Disposable secret value binding or missing-secret admission failure |
| `capture-abort`, `reply-relay` | `cases/capture-abort cases/control-plane-outage` | Exact frozen idle-source continuation under reply loss, cancelled queued work and later healthy restore; dedicated host fault relay |
| `secrets`, `deployment` | None (already initialized scope) | Focused management API contracts |

`computer-durability`, `computer-restore`, `fault-probe`, and `datapath-network`
contain guest fixtures for provider-owned assertions. The scheduled fixture is
likewise consumed by provider validation. Fixture imports are part of the selected
source closure; for example `sessions` and `persistence` import the shared
Computer from `agent`.

These sources target the current Agent/Session API. Typechecks and local helper
tests do not qualify a deployed CP/Worker/guest combination. Separate Session
questions prove answer isolation. Ordinary timers prove elapsed active execution
and cancellation, without claiming a managed wait or checkpoint. Handler
validation failures use the `handler_failed` contract.

## Prepare and run

```sh
nix develop -c python3 tests/e2e/prepare_project.py /private/case-project \
  --fixtures cases/agent
helmr deploy /private/case-project
HELMR_EVIDENCE_DIR=/private/attempt/agent \
  bun run /private/case-project/cases/agent/run.ts
```

Use normal `HELMR_API_URL` and `HELMR_API_KEY` for the explicitly selected scope.
The preparation command builds/packs the local SDK without npm publication; use
`--sdk-packages DIR` to reuse exact previously packed SDK/proto tarballs. Select
only fixture directories needed by the case. It creates a new outside-checkout
project, excludes case drivers from the deployment, and installs dependencies.
Rerun preparation into a new directory after source/SDK changes.

Host-observing `sessions`, `persistence` and `network` cases run on the dedicated host
and require `HELMR_RUNTIME_HOST_TOOL` to name its installed `dev/runtime/host.py`.
Other cases use only the native API and can target an explicitly authorized endpoint.
Different-Computer helper calls require at least two available VM slots: a hot
parent can retain its slot while its helper runs. Before selecting `helpers`,
check the dedicated Worker's advertised capacity. On a sufficiently sized host,
`WORKER_CAPACITY_VCPUS=4`, `WORKER_CAPACITY_MEMORY_MIB=4242` and
`WORKER_EXECUTION_SLOTS=2` allow two default
2-vCPU / 2-GiB VMs; the backing disk and assigned NBD devices must cover both.
Configure this before enrollment, following the host profile's replacement rules.
Cases requiring Secret management or Computer exec need those exact API-key
permissions; do not broaden an existing key merely to run every case.

The evidence directory must be new and its parent must exist. The shared `verify`
driver records `result.json`, including exact Session, Turn, question and Computer
IDs, assertions and cleanup outcomes. Physical persistence observations are also
saved separately. Cleanup cancels every owned Session, including those whose
Turn already completed, before deleting owned Computers. An open Session can
outlive a completed Turn. Observed Deployment IDs do not imply ownership.

On SIGINT or SIGTERM, `verify` writes a failed receipt synchronously with known
resource IDs and exits without racing cleanup against the interrupted body. The
caller must retain that receipt and finish owned-resource cleanup. Accepted delete
requests do not establish storage reclamation or environment retirement. Preserve
failure evidence and diagnose the actual boundary before retrying.

Use the [host runbook](../../dev/runtime/README.md) for matching installed artifacts,
service updates and edited-initial-migration reset. Reuse a healthy host throughout
the repair objective, clean up its test fixtures, then stop compute. Provider
provisioning, retained storage costs and final retirement belong to deployment
operations. A previous passing case does not qualify a changed case or artifact.

## Execution policy qualification

Selecting `cases/execution-policies` for deployment runs public HTTPS preparation
probes against `checkip.amazonaws.com`. The driver additionally requires
`HELMR_EXECUTION_POLICY_PUBLIC_PROBES=1`, opting in to `checkip.amazonaws.com`
(HTTPS and HTTP), `example.com` (HTTPS and DNS), and the public DNS resolver
`1.1.1.1`. The ordinary-connectivity baseline must establish these paths, including
numeric TLS without SNI, before their denial counts as policy evidence. Each
probe has a five-second bound. An unavailable public prerequisite fails the case;
it is not evidence of an enforcement failure or successful containment.

The case needs capacity for two resident Computers after its baseline is deleted,
Computer Hibernate/Wake and cleanup permissions, and caller-side authority to
create an owned child on the caller Computer. Its target Agent deliberately has a
different default Template. The child shares the caller's Computer by design;
these assertions do not claim hostile-process isolation on that Computer.

The driver records published pins, prepared-file markers, managed admission IDs,
network decisions and stable Session state across explicit Hibernate/Wake. It
repeats network and scoped-descriptor probes after continuation. These are native
socket/API observations; host packet-level, private-service, mandatory-deny and
cross-host qualification remain separate. Source typechecks and compiler analysis
do not execute the case or qualify a Worker/guest release.

The separate `cases/execution-policies/lifecycle.run.ts` driver reuses these same
Agents for exact host-observed restore. Prepare/deploy `cases/execution-policies`,
then run on the dedicated host with normal API credentials, a new evidence
directory, `HELMR_EXECUTION_POLICY_PUBLIC_PROBES=1`,
`HELMR_RUNTIME_HOST_TOOL` naming the installed `dev/runtime/host.py`, and exact
`HELMR_WORKER_RESOURCE_ID`/`HELMR_ENVIRONMENT_ID`. It requires noninteractive sudo
for the existing read-only observer and Worker journal, two resident VM slots,
and ordinary Computer Hibernate/Wake permissions. Set
`HELMR_EXPECT_PREPARED=warm`, `cold`, or `zero` using the prepared-startup
prerequisites below; the driver never changes Capacity policy or caches. Warm
also requires `HELMR_EXPECTED_PREPARATION_BUILD_ID` for the finite Template with
the coordinator Program and `HELMR_EXPECTED_BLOCKED_PREPARATION_BUILD_ID` for the
blocked Template with the ordinary-probe Program. Each must match the published
image and 1-vCPU/1-GiB shape. Actual adoption is checked against the exact Worker
journal phase, in addition to a fresh ready-leaf observation.

```sh
HELMR_EXECUTION_POLICY_LIFECYCLE=hibernate \
  bun run /private/case-project/cases/execution-policies/lifecycle.run.ts
```

Select `hibernate` for ordinary driver-owned Hibernate/Wake, or `drain` for
operator-controlled logical Host replacement on the same retained physical host.
For `drain`, wait for `ready-for-drain.json`, invoke native
`worker drain --wait-timeout 10m` using the profile's Worker environment, and
require successful drain plus `capture-observed.json`. The driver allows 15
minutes for drain capture, including time to observe the marker and start drain;
Hibernate capture retains its 10-minute deadline. Only then stop/start
`helmr-worker.service` and require native readiness. Atomically write
`worker-restarted.json` in the evidence directory, copying exactly `computerIds`
and `sourceHostId` from `capture-observed.json`. The driver independently requires
a fresh active logical Host on the same resource before waking either Computer;
it never stops services. A drain timeout does not authorize killing Worker.
On failure restore the Worker through the normal runbook before completing
recorded Session/Computer cleanup; marker files cannot resume an interrupted case.

Both modes retain startup/seed observations, exact captured membership and disk
save, source fencing, consumed checkpoint, reconciled controls and destination
lease/Instance evidence. They repeat finite and blocked network probes and the
coordinator's retained narrowed-descriptor, alias/discovery, empty-scope and
receipt-denial checks after restore. Run each preparation/lifecycle combination
as a fresh case. This does not qualify physical cross-host replacement, CP-outage
recovery, concurrent sibling seed deletion, or packet-level/private-service denies.

## Deployment pins and Program replacement

`cases/runtime/program-replacement.run.ts` retains one Computer across two normal
Deployment promotions. Prepare the ordinary project and a second isolated copy
changing only `runtimeSmoke`'s returned report to include `programRevision: "next"`.
Keep identical Computer images/specifications and different Program identities.
Set `HELMR_NEXT_BUNDLE_DIGEST` to the second bundle digest, deploy the original,
and start the driver. After `ready-for-deploy.json`, promote the second bundle
through the normal CLI. A second Turn in the original Session must retain its
original Deployment and code. A new Session must use the promoted Deployment and
new code while reading the prior Computer writes. This does not require replacing
the VM. Restore the ordinary deployment afterward; deployment orchestration stays
outside the driver.

## Real PostgreSQL

Run a focused top-level Go test against fresh real dependencies:

```sh
nix run .#ci-postgres -- '^TestWorkerDrainReauthenticatesDuringActiveWork$' ./internal/controlplane
```

The argument is a top-level Go regular expression, followed by one or more packages.
Every selected package must execute a passing test. Selected skipped assertions,
missing tests, package/build failures and ordinary test failures return nonzero.
Slash-separated subtest patterns are rejected; select the containing top-level
case instead. Tests remain responsible for meaningful assertions.

No arguments retains the source repository's explicit PostgreSQL CI suite. It is
not an implicit requirement for every correction. The selected invocation proves
only its executed criteria, not all service behavior or a deployed VM boundary.
Dependencies are fresh for each invocation; these short-lived service fixtures are
not the long-lived real-VM environment used across a repair scope.

For tests that need no services:

```sh
nix develop -c scripts/test-go-selection.sh '^TestAgentComputerSourceAbortPreservesLateProbeAndHeldState$' ./internal/guestd
```

Adding a test requires no runner change. Deleting one requires updating callers and
recording which obsolete requirement it removes or which replacement covers it.
Do not keep aliases solely to preserve a retired test name. Prefer a regression
that fails for the broken behavior over assertions that merely mirror the code.
Compare broken and corrected revisions with equivalent fixture state.

## Maintaining cases

Add or delete the ordinary case file with its requirement. Update needed callers
and the relevant selection guidance when deleting; keep neither obsolete aliases
nor a compatibility runner. Run source checks for changed assertions, then the
selected live case when the claim needs a real guest. An all-suite run is not a
substitute for selecting the correct boundary.

`cases/computer-restore` supplies parent and helper Sessions sharing one Computer
for provider-controlled replacement verification. The first parent Turn spawns a
helper, waits for its first Turn to complete, and returns both memory nonces and
the helper Session ID. Only then can both idle Sessions be captured. The verifier
must observe both members in one ready checkpoint, source reclamation, a replacement
Host and exact checkpoint/disk lineage. Enqueue a second parent Turn; it enqueues
the helper's second Turn and verifies the helper's post-restore file write. The
parent emits `phase: "restored"` and waits for a `"finish"` Turn message so the
provider can inspect the restored allocation before another idle capture.
`computer-durability` uses the same completed-first-Turn boundary and second-Turn
observation gate (`phase: "durability-restored"`). These gates have the fixture's
Turn deadlines. One-hour restore Turn budgets include bounded host replacement.
Deployment owners own provider retirement, baseline restoration and cleanup.
Elapsed time or completed Turns alone do not prove checkpoint restore.

## Dedicated-host drain and observation outages

These cases run on the dedicated host with `HELMR_RUNTIME_HOST_TOOL` naming the
installed source's `dev/runtime/host.py`. The native `observe` command executes
only named, typed, read-only Product observations. Match source and data generation
before using it. Each case requires its own fresh evidence directory. Fault
injection and cleanup are operator actions within the authorized exclusive scope.

For `cases/planned-drain/run.ts`, launch the ordinary driver first. After
`ready-for-pause.json`, stop `helmr-verification-dispatcher.service` and verify it
is inactive. Write `dispatcher-paused.json` with exactly the observed `computerId`, using a
private temporary file and atomic rename. Pause promptly: a source already captured
by the ordinary idle path is rejected. Do not run competing `host.py` commands
during a case; its observations use the existing exclusive profile lock.
After `command-queued.json`, invoke native `worker drain --wait-timeout 10m` using
the profile's Worker environment. Keep CP running. Require successful drain and
`capture-observed.json`, then stop/start `helmr-worker.service` and start Dispatcher.
The driver checks the pending Command survives capture, the source is reclaimed,
and the completed Command reads the original file on a different logical Host,
restored from that checkpoint and private disk version. A timeout is failure and
never permission to terminate an undrained Worker. On failure restore Dispatcher,
settle/cancel the recorded Command, and delete the Computer through native APIs.

For `cases/control-plane-outage/run.ts`, wait for `ready-for-outage.json`, stop
only `helmr-verification-control-plane.service`, hold it inactive for 150 seconds,
then start it and require `/readyz`. Keep Dispatcher and Worker active throughout.
The driver records Host observations during the real outage, requires uninterrupted
Dispatcher identity, and checks the same Session process, lease, Instance, memory nonce
and file afterward. It also requires the same active Host and fresh observations
at least 130 seconds after readiness returns. Always restart CP after an orchestration failure before fixture
cleanup; interrupted receipts contain the owned Session, Turn and Computer IDs.

For `cases/drain-renewal/run.ts`, allow approximately 40 minutes on the already
authorized host. After `ready-for-drain.json`, invoke native
`worker drain --wait=false` using the profile's Worker environment. This returns
after CP accepts the drain request. Keep Worker and all profile dependencies
alive. The Agent uses ordinary JavaScript timers for 36 minutes, preserving its
memory/file marker; it never requests a managed wait. The driver requires the same
running lease and Instance, fresh Host observations and uninterrupted Worker, CP and Dispatcher processes
through at least 31 minutes after observing drain, then the same Turn's
successful output. Require the result and ordinary Computer deletion to settle,
then require the same Host's non-null `termination_ready_at` from the native
`worker-state` observation and its matching local `drain-complete` marker before
stopping/restarting Worker. On failure,
cancel the owned Session and delete its Computer through normal APIs; a timeout never
authorizes killing admitted work.

After all fixtures are physically reclaimed, run
`sudo python3 dev/runtime/check_fencing_outage.py --evidence /private/new-dead-host-case`.
This is a separate all-dead outage after the live-Host case. It requires an idle,
ready dedicated profile, stops CP and kills the idle Worker,
keeps Dispatcher active for a 150-second outage, then proves the dead Host is fenced
only after the new healthy observation window. It restores CP/Worker in `finally`, including SIGTERM, and persists an incomplete
receipt before fault injection. Allow at least 900 seconds for native stop cleanup.
A hard kill cannot run cleanup: read the incomplete receipt and restore the exact
CP/Worker units before proceeding. The healthy-window assertion uses the same
Dispatcher invocation’s recorded `healthy_since` and the database `lost_at`,
rather than counting process startup or readiness polling as healthy time.
Run this within an inspectable native systemd operation, not an untracked background
shell. This check does not create a guest, change capacity or affect managed AWS.
None of these source-prepared cases is live-qualified merely by passing typechecks.

## Prepared Computer startup

Prepare `cases/prepared-startup`; its imported runtime fixture uses a 2-vCPU,
2-GiB Computer. Set policy through the normal Capacity HTTP API before the case.
Run with `HELMR_EXPECT_PREPARED=warm`, `cold`, or `zero`, plus
`HELMR_WORKER_RESOURCE_ID` and `HELMR_ENVIRONMENT_ID` for the exact dedicated
host and fixture. The key needs Secret management and Computer execution.
Warm runs also require `HELMR_EXPECTED_PREPARATION_BUILD_ID` for the exact
qualified build matching the fixture image, Program and 2-vCPU/2-GiB shape.
They wait for that build in a current ready-leaf observation before creating each Computer;
the case requires the Worker journal's actual adoption phase, not readiness alone.
Zero runs require no optional VM or disk occupancy and no prepared adoption.
Cold runs require ordinary startup; arrange the intended cold artifact state
through the environment owner before invocation. The driver does not change
policy or delete caches.

The case creates each secret after the ready observation, starts a Session as the first workload, and requires the fresh secret in
initialization. Initialization exclusively creates its entry marker before reading
secrets, so a prior invocation cannot be silently reused. A Command-only first
workload has no selected Agent Program and is not eligible for prepared adoption. Two sequential Computers verify distinct initialization
nonces and private writes while exercising refill between them. Journal phase
times, observations and request-to-first-Turn-result samples are retained in the
normal case result. Use the separate seed-reuse case for concurrent sibling
deletion isolation and the Session persistence cases for personal continuation.
