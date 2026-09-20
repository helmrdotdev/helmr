# Session lifecycle qualification

The package 1 candidate starts at product
`2488a8a44f7f4c3ced610d7618e850c2fae7d267`. The selected
[contract](contract.md) is the target; existing runtime/SDK/CLI support must not be
inferred from this document. Packages 2–5 complete the coordinated prerelease cut.

## Package 1 acceptance

Use the existing running Actor checkpoint fixture: admission, worker placement,
entrypoint and Workspace/lease authority come from their owning operations. Extend
the actual input delivery, worker output and turn-commit transactions; do not prove
a second in-memory or test-only lifecycle model. Physical worker observations and
CAS captures supplied by that fixture are test inputs, not native VM execution.

| Case | Required observable assertion |
| --- | --- |
| Stop before prepared settlement | Accept stop after capture preparation; later complete/fail cannot advance input/Workspace head or emit a success event. Stop is still nonterminal. |
| Settlement before stop | Exactly one terminal event references the committed Workspace head and advances exactly one input; stale stop cannot change that outcome. |
| Stop before approval output | Keep the caller's AbortSignal unnotified; the real worker append still rejects. Local belief that the Turn is active supplies no authority. |
| Approval output before stop | One output event precedes interrupt intent; stop must remain nonterminal even if a successful append response arrives later. |
| Replay | Same producer and still-writable Turn can reconcile one append; changed producer/attempt/generation or stopped Turn cannot return current write success from a historical event. |
| Ambiguous transport | Drop/delay the actual worker response after transaction commit. Absence of acknowledgment cannot produce allow; retained output proves history, not renewed native request authority. |

A delayed native start is covered here only at the admission boundary: the stop
receipt/event must not claim it has converged. Full native callback/process ownership
and forced physical exclusion remain package 3/5 qualification. Do not add a fake
native state machine to turn this limit into a claimed success.

## Remaining integration gates after package 1

- Public automatic send/enqueue/exact message routes, readiness/delivery, schema
  disposition, close/resume/recover and auth/scope behavior from the contract.
- Turn-owned Token associations across checkpoint/park/resume, other Token consumers,
  stopped-work late completion and owned child/Workspace handback.
- Explicit runtime Turn lifetime and callback/output settlement barriers; remove
  receive/Run-return implicit settlement and uncertain automatic replay.
- Actual managed process-tree stop, native pending-response convergence, physical
  Workspace exclusion and valid/invalid checkpoint continuation.
- New SDK, CLI, console and interface samples using exact candidate-generated
  protocol/runtime/package artifacts, including required/absent result typing.
- Remove old input/output surfaces and transitional storage names before the single
  integration release. No compatibility aliases, duplicate writes or backfill.

## Package 1 implemented boundary

The existing input row now owns Turn identity, execution binding and terminal-event
reference; the Session row owns active Turn and dispatch hold. Real input delivery
activates it. Worker output uses the new event relation, and explicit completed/failed
settlement shares the existing physical Workspace transaction. Old output/cursor
writers and their obsolete fake-store tests are removed, with relevant coverage
using the existing checkpoint fixture. Run return cannot advance the input cursor.

Legacy cancellation and uncertain active-Turn recovery cannot bypass an active Turn
or hold. Valid same-attempt checkpoint continuation retains its actual restored
writer checks. The full stop/recovery convergence path, final relation rename,
Session-scoped output, remaining event kinds and public/runtime/SDK surfaces still
belong to the remaining packages. This intermediate candidate is not deployable.

## Package 1 results

Package 1's contract and transaction/transport slice is complete on base
`2488a8a44f7f4c3ced610d7618e850c2fae7d267`. The final internal source tree is
`8d78a092647bdfa3ed22ad8bdb5db3d4768cc9cf`. The integration checkout's 50 intended
source paths match the writer's frozen files byte for byte. Manifest SHA-256:
`69b2f0d8a09ef8b7bd2db01137a01c9fe591a2a2e1bda9e3362e3374cdc37aad`.

Validation used the pinned Nix environment: Go 1.27.1 on darwin/arm64, sqlc 1.31.1
and PostgreSQL 18.4. Clearing the database/skip overrides made the existing fixtures
create disposable local PostgreSQL databases. No shared database was reset.

| Evidence | Exact scope and outcome |
| --- | --- |
| Full affected Go packages | Internal source tree `16ffed767f513d0fe96399d12e8c50389083ccf4`: seven packages, 1,616 passed tests/subtests, nine declared skips, exit 0. `/tmp/session-authority-final.WHe6lX/check.log` and `outcome`. |
| Final six-path recovery correction | Final source tree above: 72 passed tests/subtests, zero skips, exit 0. `/tmp/session-authority-recovery.9L7EF8/check.log`, `outcome` and `manifest.json`. |
| Generated SQL bindings | Pinned `sqlc generate`, `git diff --check` and full frozen-manifest comparison passed without source changes. `/tmp/session-authority-generation.SwqLZ3/check.log`. |
| Contract documents | Relative local links and whitespace checked. Documentation-only evidence updates leave the tested internal tree unchanged. |

The full run's skips are seven opt-in scale/measurement tests and two Linux-only
artifact/allocation tests. Relevant lifecycle PostgreSQL tests executed. The full
suite was not repeated after the final six-path recovery correction; its affected
recovery/checkpoint behavior was requalified by the 72-test run. Unchanged-surface
evidence is reused rather than presented as a second full-suite run.

```sh
env -u HELMR_TEST_DATABASE_URL -u HELMR_SKIP_POSTGRES_TESTS \
  nix develop .#default --command go test \
  ./internal/controlplane ./internal/session ./internal/idempotency \
  ./internal/executor ./internal/run ./internal/dispatch ./internal/db \
  -count=1 -timeout=600s -v

env -u HELMR_TEST_DATABASE_URL -u HELMR_SKIP_POSTGRES_TESTS \
  nix develop .#default --command go test \
  ./internal/dispatch ./internal/controlplane \
  -run 'TestTurnRecoveryCandidatesDoNotStarveTasks|TestFresh|TestActorCurrentRunCheckpointRestoreAndRecovery|TestPlaceReadyRunRestoresCompatibleCheckpointAndBindsWait|TestPlaceReadyActorContinuationReusesRestoredRuntimeAtCurrentFrontier|TestActorCheckpointFrontier|TestRestoredActor|TestSessionTurnHoldRejectsLegacyLifecycle|TestSameWorkspace' \
  -count=1 -timeout=180s -v
```

The owning tests cover both stop/settlement orders; strict producer/replay authority;
delayed and lost actual HTTP output responses; rollback of head, event and cursor;
absent result versus JSON null through worker JSON; and real checkpoint frontier
and recovered-writer provenance. An earlier full run found a run-log checkpoint
fixture opening its next receive without explicit settlement. It now settles via
the owning `f.turn` operation and preserves both lock-order races.

## Package 1 independent review and calibration

Two fresh read-only reviewers inspected the bounded candidate and rechecked affected
corrections: `/root/turn_authority_diff_review` (correctness) and
`/root/turn_authority_calibration` (second correctness perspective and separate
calibration). The initial review tree was `b469a63b7ae85e8f57dc81a1372b71bdd7377552`;
the final reviewed tree was `353e1c7ffadf117fa60fcaafa3867b082a884390`, with the final
internal tree above. Later edits only record evidence in this document.

| Finding | Parent disposition and evidence |
| --- | --- |
| Actor guard blocked child-only expiry/failure | Fixed. Keep all ancestry locks, but reject active/held Actors only when the operation can terminate that Actor. Existing real child-expiry tests cover hot/parked Actor parents, child finalization locking, wait resolution and unchanged parent Turn/cursor. |
| Blocked Actor could monopolize recovery's bounded scan | Fixed. Both lanes apply known eligibility before `LIMIT`, then retain locked revalidation. Tests assert blocked Actor ordering and recover two Tasks on consecutive limit-one passes; held, expired/invalid checkpoint, discarded private version and exhausted budget cases cannot starve Tasks. Valid active checkpoint continuation remains covered. |
| Dead output errors/query/tests survived cutover | Simplified. Delete the unused producers, mappings, lookup query and obsolete tests. |
| Output loaded and locked the same rows twice | Simplified. Reuse already-locked rows through private validation, preserving Session-before-claim order. |

Both final correctness rechecks returned no actionable findings. Separate calibration
also returned no actionable findings: early eligibility protects bounded progress,
while locked checks protect authority against concurrent changes. No new scheduler,
persistent registry or approval primitive was added. No finding was deferred.

These results qualify database/transport authority and ordering. They do not qualify
live native callback identity, delayed native-start convergence, managed process-tree
stop, physical writer exclusion, public/runtime/SDK cutover, deployment or CI. The
later packages and native-runtime acceptance listed above remain required before
releasing the combined candidate.


## Package 2 boundary

Package 2 starts at `9451e4f72166a244571484037cb553df4599a39c` and replaces
prerelease storage, durable operations and transport consumers in the same integration
candidate. It does not add an old/new compatibility path. Public and worker admission
share the durable Session owner; public identity never substitutes for worker
execution authority.

The initial schema uses `session_turns`, message delivery state and one event timeline.
Send selects and persists one branch under the Session lock; enqueue and exact-message
operations retain their distinct meanings. Stable operation receipts retain their
selected target and business rejection across replay. Deployed schema and actual
callback handling remain runtime obligations in package 3.

Output uses explicit Session or Turn scope with immutable producer provenance.
Settlement closes message admission, accounts for delivery, and commits the terminal
event with the input frontier and proven Workspace head. Unknown delivery uses the
same recovery hold and wait revocation as execution loss. Stop, lease loss and forced
termination preserve accepted FIFO work and require repair instead of uncertain
cold replay. Valid checkpoint continuation remains a separate path.

The common Token stays independent of an Actor. Its consuming wait association is
bound to the Turn, Run and generation. Session hold and exact execution currency
fence the association; no second revoked-state column is required.
A stopped association cannot consume a late result or resume a successor. Detached
Tasks and other consumers retain their own authority.

Public Session routes, API-key permissions and the Go client use the selected
contract, including an exact null-Turn Actor Run-cancellation receipt and privileged
recovery. HTTP decoding distinguishes omitted members from explicit JSON null,
rejects unknown envelope fields and bounds canonical application data. Minimum CLI
and workerclient adaptation is included so these affected Go packages use the new
wire contract. Full CLI, console, SDK and guest/executor consumers remain later work.

### Package 2 candidate and checks

The combined candidate starts at `9451e4f72166a244571484037cb553df4599a39c`.
Final review tree: `742c50ccb9e2f3ed6d133dc909cb7327bac43724`; internal source tree:
`7800154c6e351a48e767a80098d87658b90c5b19`. The complete 156-path manifest and
full/correction diffs are in `/tmp/session-package2-rollback-candidate-i8wz_bfy`;
manifest SHA-256:
`296a144933c9b2f0dc433d484c861584bb611e44854f1295127515f92483ab54`.
The parent verified and serialized integration of the backend's 78-path and Run
owner's 19-path frozen manifests, and verified all 156 integrated paths against the
review manifest before this evidence update. Subsequent documentation edits only
record evidence; the reviewed internal and CLI trees remain unchanged.

Checks use pinned Nix, Go 1.27.1, sqlc 1.31.1 and PostgreSQL 18.4. Relevant database
fixtures create disposable local databases with database/skip overrides cleared;
no shared database was reset.

| Evidence | Scope and outcome |
| --- | --- |
| Previous controlplane, Session, Token, DB and schema | 1,228 passed tests/subtests; six explicit opt-in scale skips; exit 0. `/tmp/session-package2-composition-check-43z14ddp/{check.log,counts.json,outcome}`. |
| Final controlplane and DB | 1,170 passed tests/subtests; six explicit opt-in scale skips; exit 0. `/tmp/session-package2-rollback-check-ak6dv40a/{check.log,counts.json,outcome}`. |
| Final Run and dispatch | 179 passed tests/subtests; one opt-in measurement skip; exit 0. `/tmp/session-rollback-run-dispatch-final.7rqezswu/{check.log,verification.json}`. |
| Final focused handback and lineage proof | 61 passed tests/subtests, no skips; exit 0. `/tmp/session-owner-rollback-final.whtkrn/{test.log,outcome}`. |
| Unchanged API, auth, client, worker API/client, CLI and idempotency | These packages passed in `/tmp/session-package2-combined.FjwsiU`; later corrections do not change their owned source. The combined command itself exited 1 for stale controlplane unit fixtures, subsequently corrected and covered by the final affected run above. |
| SQL generation | Pinned `sqlc generate` left the integrated source unchanged. `/tmp/session-package2-rollback-generation-zeqp9zvw`. |

The previous five-package run tested tree
`750f64627cbea0de15f64c9046da97afeb6dbd9f`; the intervening `55cb9049` review tree
only changed a dispatch test fixture. Later corrections change dispatch, checkpoint
receipt queries and their owning regressions. The final full controlplane/DB run
above requalifies the integrated correction. Session/Token/schema and public/client
owned source and schema signatures are unchanged; their earlier evidence is reused
alongside the final compilation and focused owner checks.
For final Run/dispatch evidence reuse, the parent verified all 412 selected repository
inputs, including Nix inputs and embedded resources, against the tested checkout.
No input differs; `/tmp/session-rollback-evidence-reuse-eabs2xgj/comparison.json`
records the comparison. The final source is formatted and `git diff --check` passes.

The first Run/dispatch composition run failed one manually seeded nested checkpoint
fixture that omitted its request/acknowledgement versions. The fixture now describes
an acknowledged checkpoint; production validation was retained. The focused case
and final full Run/dispatch run both pass. Earlier failed commands are retained as
superseded evidence, not reported as overall passes.

```sh
nix develop .#default --command env \
  -u HELMR_TEST_DATABASE_URL -u HELMR_SKIP_POSTGRES_TESTS \
  go test ./internal/controlplane ./internal/session ./internal/token \
  ./internal/db ./internal/db/schema -count=1 -timeout=600s -v

nix develop .#default --command env \
  -u HELMR_TEST_DATABASE_URL -u HELMR_SKIP_POSTGRES_TESTS \
  go test ./internal/controlplane ./internal/db -count=1 -timeout=600s -v

nix develop .#default --command env \
  -u HELMR_TEST_DATABASE_URL -u HELMR_SKIP_POSTGRES_TESTS \
  go test ./internal/run ./internal/dispatch -count=1 -timeout=600s -v
```

### Package 2 independent judgments and corrections

Fresh correctness review used native Codex session
`01a0b974-2a69-73e1-8ccd-20d3501e4760` (effective `gpt-6-astra`, high).
Independent calibration used Claude session `2480a303-d792-44e0-b88e-9d28c3f9959a`
(effective `claude-fable-5-1`, high). Both assignments are read-only and continue
against affected corrections. Their initial combined review tree was
`e41ed716e68dd66c05b4579249e88f93f423e8fd`; the corrected tree is recorded above.
Initial findings are retained in `/tmp/session-package2-correctness-ng_hyxgh/final.md`
and `/tmp/session-package2-calibration-2kmf7wn3/result.json`.

| Finding | Parent disposition and owning proof |
| --- | --- |
| Failed Actor completion/checkpoint still retried or detached the Session | Fixed. Failed initialization, active execution, between-Turn checkpoint failure and no-progress return preserve FIFO/head behind a recovery hold, retire execution and permit exact repair. Clean drained return remains valid. Real failure/return/recovery cases use the existing worker transactions. Obsolete cold Actor retry queries and tests are deleted; Task retry is retained. |
| Delivered close outbox entry suppressed a later wake | Fixed. Each durable close/recover/resume transition uses its operation identity for reconciliation. The real delivery worker proves a prior delivered close cannot suppress repair/resume. |
| Drained recovered Session could not close | Fixed. A recovered/interrupted hold closes only with no current Run or active Turn, a drained cursor and physical writer exclusion. The hold clears after successful Workspace release. Queued work remains held until resume. |
| Missing exact interrupt target returned 500 | Fixed. The domain returns typed `turn_not_found`, mapped to 404; the public PostgreSQL test exercises the missing target. |
| Redundant wait-revocation state and duplicate hold writes | Simplified. Remove `turn_revoked_at`; exact Turn/Run/generation and the current Session hold fence wait use. Interrupt uses common hold provenance and fresh-acceptance graph retirement. Parsing/binding is performed once within the owning operation. |
| Actor continuation eligibility/proof was incomplete | Fixed. Exhausted Actors are filtered before bounded placement; all managed Actor checkpoint kinds use the committed Session frontier and source proof. The same-Workspace child handoff still proves its private version, using the Actor parent's current committed base. |
| Generic Task Token waiter inherited an Actor-only rejection | Fixed. A Task's nil-Turn wait retains Task lineage/Workspace authority even under an Actor-owned Workspace. Real parent commit, child handoff, placement, start and Token resolution prove isolation; a shared Token's unrelated waiter survives Actor stop. |
| Message rejection code disagreed with selected contract | Fixed. Unstarted deliveries use `turn_stopping`; uncertain started deliveries enter execution-loss recovery. |
| Adding cursor details made common errors non-comparable | Fixed. A scoped scalar cursor error preserves existing sentinel matching; existing Token expiry tests remain authoritative. |

The parent retains separate unlocked eligibility and locked authority validation:
the former prevents bounded-scan starvation, while the latter protects concurrent
transitions. Placement uses durable elapsed time; loss lanes account for the exact
physical-loss interval. Their temporal differences do not justify a SQL-function
abstraction. Prelookup authorization and final resource scope protect different
boundaries. The selected cursor-expiry envelope does not require a new trimming job.
No generic transaction dispatcher, Actor Token service or compatibility path is added.

The accepted `failed` Session status/event/diagnostic vocabulary stays in the
contract and projection. Run failure now uses a recoverable hold, so no current
package 2 path produces terminal Session failure. Remove the uncalled failure
helper and unused reconciliation query setters, rather than retaining a dead
failure writer. This does not change the selected public status vocabulary.

The final calibration also requested one computed Actor terminal decision per
completion, reuse of child-wait cursor validation and deletion of three redundant
SQL Session guards. Their sole production owners already lock the Session and
validate the same exact execution predicates; ordinary statement identity, status
and CAS conditions remain. The unlocked scan eligibility checks remain separate.


The next correctness recheck (`/tmp/session-package2-final-correctness-cs5r43gl`)
confirmed the original four fixes but found three more reachable defects: consecutive
private checkpoints were incorrectly required to directly descend from the committed
head; a historical outside-Turn Token wait was checked against a later active Turn;
and an accepted `no_progress` completion could not replay its fingerprint.
The parent accepted all three and corrected them with real PostgreSQL regressions.
One derived query follows exact acknowledged checkpoint/source-runtime provenance
back to the committed head. It retains per-edge version/source/ownership proof and
strictly decreasing writers; no persisted frontier or arbitrary ancestry acceptance
is added. Latest eligibility and live wait binding stay distinct from historical
released-wait provenance. Unchanged settlement uses the validated checkpoint or
child return and preserves the committed-head CAS.

The same source audit found normal child handback advances the writer generation,
child failure/cancel returns the original private base, and pre-placement cancellation
has no child writer at all. The existing producer operations create these distinct
receipts. Narrow branches now validate the appropriate exact receipt, including
proof that a null-child-writer path never admitted a child physical writer. The
positive/negative PostgreSQL cases cover direct settlement and a subsequent Token
wait; ancestor stop still retains its hold. No new Task failure policy is introduced.
The calibration recheck (`/tmp/session-package2-final-calibration-h34lq4fq`) found
the smaller cleanup items above; their focused corrections passed. The next review
of the 7cdbf3b6 candidate is retained in
`/tmp/session-package2-recheck-correctness-0w7gjaxy` and
`/tmp/session-package2-recheck-calibration-d36z7x3f`.

That correctness pass found two further reachable cases: an owned Task's generic
Token wait does not have the child-handoff generation fields, and nested Task
handback can legitimately use a terminal lease base different from the original
Actor handoff. The existing producer transactions distinguish immutable Task origin
from each admitted execution base. The correction retains exact checkpoint/source,
Run/attempt, ownership, terminal outcome and handback receipts; successful output
remains parented by its terminal source lease's actual base. Failure still returns
the immutable original base. No generic Workspace ancestry walker or new persisted
state is introduced. The three actual repro cases failed before the correction
(`/tmp/session-owner-nested-repro.jJfFNg`) and passed afterward
(`/tmp/session-owner-nested-correction.qzvLmp`). The corruption checks and affected
package checks pass as recorded above. Final independent rechecks use
`/tmp/session-package2-qualified-correctness-zycdlmxq` and
`/tmp/session-package2-qualified-calibration-x1ngqajt`. Calibration found no
actionable issue and accepted the bounded terminal-owner proof. Correctness confirmed
normal and nested continuation fixes, then found two additional interruption paths:
an owned Task restore lost before acknowledgement cannot redispatch, and cancellation
of a parked owned Task leaves a checkpointed source lease that parent handback rejects.
The lost-restore correction uses exact expired-restore receipts. Parked cancellation
initially used invalidated previously-ready checkpoint receipts; the final rollback
simplification below replaces that failure-source reconstruction. A resolved Token
retains its condition outcome during cancellation; the final proof checks logical
termination independently. The same existing owner also handles runtime-preparation
exhaustion. Actual reproductions failed in
`/tmp/session-owner-parked-repro.egIyNC`; corrected continuation and rejection
cases passed in `/tmp/session-owner-parked-qualified.54JpBl` and the final focused
suite. No status-only broadening or new durable state was added. Affected judgments are in
`/tmp/session-package2-parked-correctness-fqcqa4un` and
`/tmp/session-package2-parked-calibration-fj6qvzsx`. Calibration found no actionable
issue. Its optional reason-consistency trim concerned the failure-source branch
subsequently removed by the final simplification.
Correctness confirmed both fixes and found one remaining nested-cancellation case:
a cancelled child's own checkpoint writer can precede an admitted descendant's
latest Workspace writer. Resumption must prove that latest owned descendant is
excluded while retaining the direct child's checkpoint provenance. The same writers
reproduced and corrected that handback admission boundary in a frozen dispatch
slice, with actual Actor and Task parents, pre-cleanup rejection, regrant and later
settlement (`/tmp/session-owner-descendant-qualified.x0QOvE`). A further parent
source audit reproduced failed attempt 1 followed by cancellation of never-leased
retry attempt 2 (`/tmp/session-owner-retry-cancel-repro.l0OyGR`). The last physical
writer belongs to attempt 1 while logical termination belongs to attempt 2.

Before adding another source-attempt exception, the parent requested a bounded
independent design/calibration critique in `/tmp/session-rollback-proof-critique-zpxs_dgo`.
It found rollback content provenance, physical exclusion and logical terminal state
had been conflated, and judged their separation sound and smaller. The parent chose
to consolidate failed/cancelled original-base handback: keep the parent's exact
checkpoint/base proof, exact terminal child/current-attempt and handback outcome,
and scoped owned-graph exclusion with separate current-writer attribution. Successful
child output retains its full producer/current-attempt proof. Historical failed edges
consume the acknowledged parent restore and retain its base/origin/writer ordering;
they need not reconstruct discarded child contents or current physical state.
This correction is implemented. The live helper includes the direct child and all
owned same-Workspace descendants, and runs for every failed/cancelled handback with
a recorded child writer. It independently proves current-writer membership and
physical exclusion; exclusion alone cannot bypass the writer fence. Existing
reclaim receipts accept an observed-failed runtime after actual reclamation, while
live mounts, leases, processes and unexcluded or foreign writers still reject.
No new persisted state or public API is added. Actual retry-gap Actor and Task
parents, exhausted retries, reclaimed failed runtimes and subsequent Actor Token
wait/settlement pass in `/tmp/session-owner-rollback-qualified.yuSkbN` and the final
61-case focused run. Successful child output proof is unchanged.

The critique's proposed case of a persisted rejected attempt-2 Run lease without
its Workspace lease has no production writer in this candidate. The sole Run-lease
insertion caller, `grantFreshRun`, inserts both leases, advances the Workspace fence
and binds the parent child writer in one transaction. The parent classified that
specific hypothetical as non-actionable rather than inventing an invalid fixture.
The actual never-leased retry gap and reclaimed failed-runtime receipts are
qualified above. A separate null-child-writer hypothesis was also non-actionable:
the failure/reclaim writers clear runtime reservations, so reclaimed failed runtimes
do not match the reserved-runtime exclusion predicate. The no-execution branch
remains unchanged; no synthetic unreachable case justified another exception.

The parent also accepted removal of source checks already discharged by the shared
Actor lineage query and a redundant publication disjunction. Current resource
comparison, resume binding, checkpoint identity and committed-head CAS remain.
Historical wait/child outcome pairing is aligned with live admission. A proposed
virtual-current-node mode is non-actionable: live admission and acknowledged
historical provenance have different owners and state, and the bounded receipt
correction does not require another query mode. No actionable finding is deferred.

Final correctness review of tree `742c50cc` returned no actionable findings in
`/tmp/session-package2-rollback-correctness-6d5ojsrp/final.md`. Separate calibration
also returned no actionable correctness or simplification finding in
`/tmp/session-package2-rollback-calibration-19bnfc50/result.json`. The parent keeps
the optional redundant terminal/reclaim predicates in the writer-membership query:
they express the local source receipt without adding state or an alternate path;
the reviewer explicitly classified their deletion as optional, not required.
Both reviewers inspected the exact manifest and affected correction. The final
controlplane/DB command was still running when they wrote their judgments; the
parent subsequently verified its terminal exit 0 and counts above on unchanged
source. Neither reviewer claimed native-runtime validation. No performance claim
is made for arbitrary-depth owned Run graphs; native/runtime and integrated
qualification remain the later package boundaries below.

Review scope notes: interrupt followed by a leased worker's failed completion is
rejected while the hold is set. Finalizing leases cannot renew; existing lease-loss
cleanup preserves the hold/FIFO and excludes the writer. Package 3 still supplies
cooperative stop convergence. Exhausted paused Actors are excluded defensively from
placement, but current checkpoint production cannot create them: closing the active
interval checks the hard deadline before pausing, and lease recovery does not resume
an execution that exhausted that deadline. The injected exhausted-placement fixture
proves bounded-scan progress, not a new paused-Run expiry service.


### Committed versions with private ancestors

The parent checked the version-consumer boundary raised in calibration. Existing
`recordTaskWorkspaceVersion` / `PublishTaskWorkspaceVersion` already publish a
committed full snapshot whose parent is the live lease base, including a private
restored base. `TestRestoredActorCompletionAdvancesFromPrivateLeaseBase` (in
`actor_completion_restored_postgres_test.go`)
asserts a committed new head with the restored private version as its parent.
The schema permits this relation and protects the parent, artifact and source lease
with `ON DELETE RESTRICT` references.

`GetWorkspaceResetTargetAuthority` and `GetCheckpointWorkspaceBaseAuthority` load
the selected version's own artifact. Their Go projections require valid artifact
and tree identity, not a committed parent. Guest materialization and reset extract
that complete tar artifact; they do not reconstruct a chain of parent snapshots.
The only checked-in version-discard writer is scoped to a staged Workspace-exec
capture; no general version/artifact/CAS row deletion query exists in this product
candidate. This is source/DB evidence, not new native runtime or storage-retention
qualification. No collector or alternate publication state was added.

### Remaining release boundary

This package qualifies durable DB/transport authority only and is not independently
releasable. Package 3 must carry these fields and barriers through guest/proto,
executor and TypeScript runtime/SDK, including multiple explicit Turns per Run.
Package 4 completes CLI/console/examples and documentation consumers. Package 5
qualifies the exact combined artifacts and real native callback/process stop,
checkpoint/restore and physical exclusion. Those claims require their own evidence;
passing database fixtures does not discharge them. No CI, merge, publication,
shared-environment reset or deploy is claimed here.


## Package 3: execution and TypeScript contract

Candidate base: `a48b8a4fd6951931d7b0e700ca39a0f163fbdaf2`.
The parent integrates frozen SDK/compiler, runtime, and protocol/executor/guestd
slices, plus control-plane completion and worker control support. Package 3 is qualified at the source, framed-protocol and isolated PostgreSQL
boundaries below. Whole-plan integration and native execution acceptance remain
packages 4 and 5.

The public Actor contract is the custom `run(session, ctx)` loop with explicit
`session.receive()`, `turn.complete(result?)` / `turn.fail(error)`, sequential
message handlers, typed schema transforms, and tracked output barriers. Run exit
never settles a Turn implicitly. Actor outcome carries Run generation and an
exact interruption hold/nullable Turn; its obsolete terminal input cursor is
removed. The control plane uses the committed Session frontier.

Cooperative interruption retains a leased parent's capture authority, releases
only hot consuming waits, cancels owned children, and reuses the existing
quiesced-program capture/finalization proof. Mid-checkpoint uncertainty and
unacknowledged callbacks remain recovery cases. A proved child finalization
already establishes program quiescence; unproved descendant execution requires
observed physical cleanup. Exact pending cleanup receives a retryable response
within the existing finalization deadline. Stop reads are advisory single-query
observations without shared Worker supply locks. Privileged recovery remains
available only to the authenticated client.

### Evidence and integration corrections

- SDK/compiler frozen 29 paths: `/tmp/session-sdk-verified.Fa5iMD/manifest.json`.
  Both typechecks, 159 tests, packed SDK declaration metadata and compiler
  generation passed. Initial full deployment failure came from the dev-shell
  restrictive umask changing a fixture's requested `0644` to `0600`; the full
  package passes with `umask 022`, without a product or fixture change.
- Runtime final correction: `/tmp/session-runtime-settle-stop-freeze-ehg1wmyn/manifest.json`.
  Typecheck, 61 protocol/runtime tests, actual bundle generation and regeneration
  check pass. This includes pending receive stop-only binding and a null-Turn
  stop racing an in-flight final settlement response. No input or successful
  settlement is synthesized from a stop notice.
- Execution final correction: `/tmp/session-execution-completed-freeze-wdcw1xs8/owned-manifest.json`.
  Executor, guestd, wire and worker race suite: 514 passing tests, five explicit
  platform/fixture skips. Linux worker/guest cross-build passes (compile only).
  The `completed` discriminator now crosses a generated protobuf frame and the
  physical pause/capture/ready/applied handshake in a regression test.
- Combined Go run `/tmp/session-package3-combined.jsonl` initially exposed only
  an outdated route inventory and a child-call fixture that assumed the child
  existed before same-Workspace checkpoint handoff. Both were corrected.
  The subsequent full controlplane/secret/session run
  `/tmp/session-package3-final-cp.jsonl` has 944 passes and four opt-in skips;
  remaining packages passed in the combined run. Later changes receive scoped
  requalification below, rather than being attributed to an older binary.
- Integrated TypeScript preparation requires `bun install --frozen-lockfile`,
  `scripts/build-compiler-entry.sh` and `scripts/build-npm-packages.sh` before the
  packed SDK/compiler tests. Failed attempts lacking those local build outputs
  are retained in `/tmp/session-package3-typescript-integrated*.log`; they are
  superseded by `/tmp/session-package3-typescript-qualified.log`.
- Parent stopped completion, Token isolation, hot child-call cancellation,
  between-Turn cancellation, wrong hold/generation/Turn and unresolved callback
  cases pass. Read-only stop observation is tested while a different transaction
  owns the Worker Group lock. Completed different-Workspace child proof is reused
  without waiting for warm Runtime reclamation. Relevant logs:
  `/tmp/session-package3-stop-read-regression.log` and
  `/tmp/session-package3-proved-child.log`.

### Independent judgments

Fresh native Codex `/root/package3_sdk_runtime_review` runs at the Founder's
requested medium effort. P1 corrected the executor's obsolete `succeeded`
settlement discriminator. P2 identified reciprocal controls from owned Tasks in
different Workspaces; its source-Actor ancestry ordering correction is fixed and independently
reviewed. An old-order overlay reproduces PostgreSQL `40P01`; the corrected
owned-child reciprocal and child-to-parent finalization cases pass. The final
native Codex affected pass reports no remaining actionable findings.

Fable session `4c0fcb18-93ce-467a-bc0f-c197a8ae6660` supplied bounded plan critique,
then independent diff/calibration at medium effort. Raw results are in
`/tmp/session-package3-control-critique-grf7rb3s` and
`/tmp/session-package3-calibration`. C1 requires deferring in-graph child source
locking to the existing graph hook. C2 is fixed by lock-free stop observation,
retaining timely independent stop delivery. C3 is fixed by pending settlement
reconciliation. C4 prompted reuse of already-proved child finalization, with an
actual completed-child database regression. S1/S2 simplify binding comparison
and reuse locked Secret delivery under held Workspace locks, whose foreign key
prevents a late binding insert; S3's private two-call-site control flag is
retained to avoid a new orchestration abstraction. The unleased-only recovery
wrapper and explicit prelocked Resume seam remain necessary.

Native Linux cgroup/VM cleanup, external provider effects and actual VM checkpoint
restore are not proved by macOS framed/DB tests. Their package 5 acceptance remains
outstanding, as do package 4 CLI/Console/example consumers. No merge, publication,
deployment or shared-environment reset was performed.

Final affected Go race requalification:
`/tmp/session-package3-final-delta.jsonl` — 48 passes, no failures or skips;
controlplane 31.402s and secret 1.596s. Final integrated runtime typecheck,
61 tests and bundle check pass in `/tmp/session-package3-runtime-final.log`.
The final controls correction is bound by
`/tmp/session-worker-controls-correction-manifest.json`; its removal of the
standalone delivery export/query is included in regenerated sqlc output.

Final parent disposition: native Codex correctness and Fable diff/calibration
both report no remaining actionable findings. Final calibration raw result:
`/tmp/session-package3-calibration-final/result.json`. All technical source hashes
match that review snapshot; only this validation record was revised afterwards.
The optional narrower read projection is non-actionable: retaining the current
shared binding row shape introduces no state or alternate path. Failed Task
finalization also retains its operation ID: `CompleteTaskRunLease` requires an
existing finalization operation and sets only terminal fields, never clearing the
proof. The exported delivery validation helper is removed. No actionable finding
is deferred; package 5's native execution requirements remain explicitly unproved.

All four package 3 writer worktrees and branches are retained for corrections and
package 4/5 combined-candidate integration. Prior package 1/2 and design checkouts
remain retained under the HQ Handoff. No source has been merged or published.

Qualified implementation subtrees (unchanged by the validation record):

- `internal`: `ddc8d1e82bf49edd00d7d6b2b0ab7f788f1f3f6c`
- `sdk`: `bf393881e459f63671be154ce6caf18f7c311b29`
- `runtime`: `d00e51448d6f90515f24dfa405a33b1f7d9b64a6`
- `compiler`: `4c98e533a237cd549af957152ff2274c84fcaba8`
- `proto`: `55955fef26e7d07ddc8d5edb28b9bf8f313a6eb3`

## Package 4 — CLI consumer slice (2026-09-20)

Parent-owned candidate based on `cf4e273735a7fb5f443383273e4be0a1c4a6efbf`
in the existing integration checkout. The Founder reviewed the CLI proposal and
approved ordinary `resume SESSION_ID` selecting the current hold once, then
submitting that exact ID. Optional `--hold` binds an explicitly observed hold;
stale selection never retargets. Recovery continues to require explicit hold,
Turn or outside-Turn identity, Workspace version and reconciliation reference.
SDK, API and runtime contracts are unchanged.

Added CLI enqueue, exact Turn get/send/interrupt, resume and recovery; updated the
Actor CLI reference. Receipts retain server status (including stopping), Turn
outcomes are separate from Session outcomes, and out-of-Turn recovery preserves
an explicit null Turn ID. Full CLI package tests pass under:

```sh
nix develop .#default --command sh -c 'umask 022; go test ./cmd/helmr -count=1'
```

Result: pass, 6.333s. HTTP fixtures check exact targeting, application JSON,
recovery requirements, receipts, failed Turn outcomes, observed-hold resume,
stale-hold rejection without a second read/mutation, and refusal to automatically
resume recovery-required or unheld Sessions. Actual `go run ./cmd/helmr actor
resume --help` and `actor recover --help` also pass. `git diff --check` passes.
The initial new JSON-output test expected pretty-print whitespace; its assertion
was corrected to the existing compact JSON output without product changes.

Fresh combined correctness/simplicity review by `/root/session_cli_review` at
medium found one P2 documentation issue: close drains accepted FIFO work rather
than interrupting it. Corrected the reference to describe draining and retained
holds. No code or unnecessary-mechanism findings were identified.

Reviewed source SHA-256:

- `cmd/helmr/actor.go`: `3f69e968dec94673479d639dcb183840be127ac6c5e2502d31b829bbfe501f69`
- `cmd/helmr/actor_controls.go`: `468be25676879e48745c3ad0c53a74639fdd2a22862edc8a6c2e17788f6a1b4a`
- `cmd/helmr/actor_controls_test.go`: `7f845a4cb244f63fdd3b5663cb44e37261a3194ed78b9fe70205acf55f94fd0f`
- Corrected CLI reference: `4734c28816aaadf81912764338c31013912c08f2bfd35e47facdd25c9d05993b`

These are CLI fixture and native help checks, not deployed authorization,
real runtime convergence, browser acceptance or VM evidence. Console, remaining
first-party callers, examples and remaining website documentation still require
package 4 work; native integrated qualification remains package 5. No new worktree
was created; the existing integration and HQ plan checkouts remain active, and
previous frozen writer worktrees remain retained under the HQ Handoff.

## Package 4 — Console consumer slice (2026-09-20)

Parent-owned candidate at CLI base `cdf34ca65ba9d9adf8fa2d62a44cb12ac02f9290`.
Replaced separate input/output queries and timestamp interleaving with one retained
event sequence. Added Turn inspection, result/error/readiness, Session send versus
FIFO enqueue and exact Turn messages, exact interrupt/resume confirmations and
operation receipts. Session closing continues polling; terminal Session observation
performs a final dependent read of events, Turn and Run history. The last observed
active Turn remains inspectable after its active pointer clears. Session/scope
navigation remounts local mutation state; confirmation targets and retry keys stay
bound to the originally selected Turn/hold. Recovery-required holds direct operators
to explicit CLI reconciliation. No recovery override or SDK changes were added.

Updated closing status and API-key lifecycle grant options. Replaced obsolete demo
records with five ordered events and two completed/failed Turns, demonstrating that
a failed Turn can coexist with an open Session. Demo seed uses the current event
allocator and succeeds against a fresh isolated PostgreSQL database.

Evidence:

- `bun run --cwd packages/console typecheck` passes, including final correction.
- `bun test packages/console/src`: 93 pass, zero fail; log
  `/tmp/helmr-console-checks.log`.
- Console production build passes. Final tested embedded bundle is
  `index-BP7MJnIn.js`, stylesheet `index-BwHXMLXK.css`; stack build log
  `/tmp/helmr-console-stack-final.log`.
- `env -u HELMR_TEST_DATABASE_URL -u HELMR_SKIP_POSTGRES_TESTS go test
  ./cmd/internal/dev-controlplane -run 'TestDemoEnvironmentSeed|TestDevSeedRestart'
  -count=1` under pinned Nix with umask 022 passes (2.059s).
- `HELMR_E2E_BASE_URL=http://127.0.0.1:18420 bun run test:browser
  tests/browser/session-lifecycle.spec.ts tests/browser/console-demo.spec.ts`:
  4 pass (2.7s), final log `/tmp/helmr-console-browser-final.log`. Two real demo
  tests exercise local login, scope, seed and rendering. Two response-controlled
  UI regressions exercise stale projections at terminal transition, interruption
  receipt versus convergence, exact confirmed hold despite subsequent polling,
  and retained idempotency on retry.
- Native in-app browser at the same isolated local stack verified the five-event
  order, failed Turn with open Session, rejection of a late exact-Turn reply,
  and a real enqueue receipt followed by event sequence 6. Screenshot inspection
  found no overlapping/overflowing controls at the browser's current desktop size.
- Fresh combined review `/root/session_console_review` at medium found two P2s:
  final projections could remain stale after terminal Session polling stopped;
  a durable interrupt-request flag could show waiting copy after interruption
  had converged. Both are fixed with focused browser regressions. Correction
  review reports no remaining actionable findings or unnecessary mechanisms.

Qualified SHA-256:

- Session detail: `9e9a2ab893b1536a56ae55ef1ed8504025eb6b72600f65f93c2a8b0c7cb1eead`
- Session API helpers: `3544eb1791876db174e23a4d5cbb2fe453a245eb40e74f2442f48e117f6d50f7`
- Browser regression: `136b36c6ee25ebd2b189178c3669a1ecf3ed48dd83d3638e0097cc5ec13f6af9`
- Demo seed: `68eeee18d4cc67f7e43d8fa7f9067f1e9dd1e5339ddf0dc68f307a95b6ca38df`

The owned local environment `/tmp/helmr-console-lifecycle-20260920` used separate
PostgreSQL/Redis/ClickHouse state and no shared DSN; its stack and temporary browser
were stopped after verification. Keep its state/logs for reproduction. No worktrees
were added or removed. Existing integration/HQ and frozen writer checkouts remain
retained under the Handoff. UI fixtures do not prove physical interruption,
checkpoint recovery, provider side effects, production authorization or every role;
those runtime boundaries retain prior evidence and outstanding package 5 acceptance.
First-party smoke/native fixtures, editable examples and remaining website docs
still require package 4 work. No merge, push, deploy or publication occurred.

## Package 4 — existing consumers, small examples and documentation (2026-09-20)

Parent-owned slice at base `3a4583eb2344da7ab46255acbfb43edfd1718be0` migrates
child-Task/Actor smoke code to separate start/enqueue, explicit Turn completion,
exact Turn idempotency and shared event pagination filtered for application output.
The single-Turn smoke Actor intentionally returns after settlement, retaining its
existing separate continuation Run check. Management smoke passes cancellation
transport options in the new third argument. SDK/runtime/API source is unchanged.

`examples/hello-world/tasks/session.ts` adds finite output followed by deterministic
validation and required typed result, plus external CI through generic Token
creation/wait/completion. Completion transport failures remain outside business
failure catches. README describes stop/Token-completion races, queued work, exact
hold resume and the lack of automatic provider-socket durability. These examples
are typechecked; their actual VM execution is not claimed.

Packed SDK consumer now validates RecordWriter, typed Actor input/message/output/
result and no-result completion from the actual npm archives, then runs Session
enqueue/exact-message/event requests under Node against a bounded fake transport.
Updated Actor concepts, how-to/tutorial/reference, wait guidance, homepage examples
and stale diagnostic wording. Removed old first-party input/output API use; a sweep
across dev/examples/scripts/web/tests/fixtures finds none of the removed methods.
The homepage native provider snippets remain editable illustrations, not a newly
qualified provider integration.

Validation under pinned Nix:

- Compiler entry and npm builds, local SDK sync/frozen installs, workflow typecheck
  pass. The first client typecheck exposed the obsolete cancel options position;
  corrected client and all example project typechecks pass in
  `/tmp/session-consumers-qualified.log` before its superseded Astro failure.
- `scripts/check-packed-sdk-consumer.sh` passes actual npm archive extraction,
  TypeScript compilation and Node execution; `/tmp/session-packed-web-checks.log`.
- `bun test tests/web-messaging.test.ts`: 144 selectable compositions parse.
  Root `test:ts` includes this regression. This proves syntax only, not provider
  SDK signatures or execution. `/tmp/session-web-qualified.log` also records
  Astro check with zero errors and the successful 64-page build/link check.
- Final comment-only readiness clarification rebuild passes all 64 pages and
  link checks in `/tmp/session-web-final-build.log`. The initial trailing-slash
  link and initial placement of the Bun test inside Astro src were corrected;
  neither failed run is counted as final evidence.
- Native in-app browser inspected the local built SDK reference at port 18430,
  showing receive/explicit settlement, arbitrary messages, event cursor and exact
  stop/resume/recovery contracts. The preview and temporary tab were stopped.
- Fresh medium combined review `/root/session_consumers_review` found duplicate
  provider/Actor variable names and stripped interface routing fields in composed
  homepage examples. Native identifiers and explicit routing fields correct them;
  the all-combination parser covers syntax collisions. Final review reports no
  remaining actionable findings or unnecessary mechanisms.

Remaining accepted work: editable Codex app-server and Claude live issue-fixer
implementations and their source/protocol qualification, followed by actual native
VM smoke/checkpoint/stop/recovery acceptance. Static smoke checks do not establish
those boundaries. Generic CI/finite-output examples do not substitute for the
required native human-interaction example. No new worktree was created; existing
integration/HQ and retained frozen worktrees remain as recorded. No merge, push,
publication, deployment or shared-environment reset occurred.

## Package 4 — editable native issue-fixer samples (2026-09-20)

Parent-owned application slice at base
`7229dc5b858b44fc775a7f86b9e907139e5e36a0` adds separate Codex app-server and
Claude SDK Actors under `dev/workflows/tasks/issue-fixer`, plus an authenticated
Slack interface under `dev/workflows/interfaces`. No SDK/runtime/REST primitive
implementation changes. Each Actor installs message readiness, publishes exact
native requests, uses a fresh scoped permission-admission write before allow,
invalidates cancelled callbacks, streams output, waits for the direct native process
to close and runs fixed repository checks before explicit settlement. Claude
follow-ups are serialized after each native result; Codex uses exact native steer.

Slack verifies raw-body signatures and a configured app/team/channel/user binding.
New issues enqueue; replies and stops retain their original Turn. Intake uses the
Slack event ID as the idempotency key and only ACKs successful admission. Finite
Slack retries are not indefinite durable ingress. Retained output updates one
preconfigured bot message before persisting its cursor; an uncertain response
repeats the same update rather than posting another message. The README explains
mounting, commands, current-hold resume, limited status projection and failure limits.

Qualification:

- Pinned Nix workflow typecheck and nine targeted tests pass in
  `/tmp/session-issue-fixer-checks.log`. Run the two test files separately because
  the native-boundary fixture mocks the provider module: `bun test
  dev/workflows/tests/issue-fixer.test.ts` and `bun test
  dev/workflows/tests/issue-fixer-native.test.ts`.
- Tests cover out-of-order/duplicate/stale replies, wrong question keys, stop-first
  rejection before local abort, ambiguous admission writes, native cancellation
  during admission, signed interface authorization/immutable targeting/text decoding,
  and cursor retention after uncertain Slack update. A real delayed subprocess
  fixture exercises the actual Claude Actor handler: two rapid follow-ups, direct
  exit before checks, then a failed check rather than completion. A Codex transport
  fixture invalidates a request while notification projection is blocked.
- Codex 0.133.0 generated TypeScript protocol in
  `/tmp/helmr-issue-fixer-codex-protocol` confirms request/answer and startup schemas.
  Actual pinned binary `initialize`/`initialized` handshake passes in
  `/tmp/session-issue-fixer-codex-handshake.log`; no thread, model or login request
  was sent. The old binary reports an incompatible host config field and falls
  back to defaults; this startup evidence does not qualify model configuration.
- Claude 0.3.149 declarations and current official docs establish callback input,
  answer mapping, the process-spawn hook and non-awaitable `close()`. Sources and
  exact installed versions are linked in the sample README.
- An initial Bun assertion form attached two pending rejection matchers to the same
  error and spun the test process. The owned processes were stopped; collecting
  the rejection before asserting resolves it. Only the final successful log counts.
- Fresh medium combined review `/root/issue_fixer_review` found three application
  defects: non-awaited Claude exit, Codex invalidation blocked behind output, and
  undecoded Slack text entities. All are corrected with boundary regressions;
  correction review reports no remaining actionable findings or unjustified
  abstractions. No provider adapter interface was added.

The Founder explicitly requested unbiased primitive reassessment while building
samples, including `session.send` versus `enqueue`, and a return for decision before
implementing any proposed public primitive change. The review finds distinct FIFO
admission and exact interaction necessary. Atomic automatic routing can serve a
same-meaning conversational input but is not exercised by this issue fixer; neither
its removal nor broadening follows from this sample. Native callbacks remain
separate from durable Tokens. No additional permission primitive is established by
the application corrections. These observations are not a permanent API freeze.

Remaining proof: actual native model requests/responses and permission effects,
Slack delivery/ACK latency, A/B queue/stop/late-reply/current-hold resume in the
runtime, descendant exclusion, remote-effect reconciliation and checkpoint/restore.
Direct child closure is not proof of those boundaries and cannot justify ordinary
success with known unresolved work. Full native execution acceptance stays open;
this is a locally validated sample slice, not completed package 5 qualification.
No new worktree, paid inference, real outbound message, merge, push, publication,
deployment or shared database reset occurred. Existing worktrees remain retained.

## JSON-only Actor and admission reassessment (2026-09-20)

The candidate based on `ee9f84a1c52611dfc73fe1b296d285c8988126a6` removes
Actor schema authoring, inferred reference generics, compiler manifests/slots and
runtime validators together. Task payload and Token wait validation remain. All
Actor boundaries carry application JSON. Existing schema-preservation tests were
removed; the sequential-handler test now exercises application-owned validation
and MessageRejected rather than runtime schema interpretation.

Admission now checks logical active-Turn authority separately from worker delivery
readiness. It accepts pre-handler and parked messages, rejects settlement-first
admission with turn_settling, and never changes their selected Turn. Reads advertise
logical acceptance. Settlement and stop retain explicit unstarted-message rejection.
No migration, compatibility API, new wake mechanism or provider adapter was added.

Evidence for this delta:

- SDK/compiler typechecks pass; 157 SDK/compiler tests pass under pinned Nix
  (`/tmp/session-json-sdk.log`). Runtime typecheck and 60 tests pass
  (`/tmp/session-json-runtime.log`). Host Node 23 could not run the project's newer
  native APIs; those early host-runtime failures are superseded by pinned checks.
- Deployment, Session and full controlplane package tests pass
  (`/tmp/session-json-go-full.log`, controlplane 218.674s). The combined command
  also mistakenly named `internal/runtime`, which has no Go source; that setup error
  is not a runtime test or a package pass. TypeScript runtime proof is above.
- Two additional real-Postgres tests pass (`/tmp/session-json-park.log`): acceptance
  while physically parked does not resolve the original Token or create a lease;
  after normal resume, the same message is delivered to the same Turn. A never-
  registered handler's pending message is explicitly rejected at settlement.
- Final affected controlplane race checks pass, 16.117s, with all example
  typechecks (`/tmp/session-json-final-delta.log`). They include message settlement,
  parked delivery, Token/stop ordering and Turn authority cases.
- Workflow typecheck and 7 interaction/interface plus 3 native-boundary tests pass.
  The latter include two separate Claude Actor handler invocations sharing only
  Workspace state, with the second query receiving the native resume ID. The mock
  provider proves wiring, not model history or native crash persistence.
- Packed npm SDK consumer and all 144 website snippet compositions pass
  (`/tmp/session-json-consumers.log`). Website typecheck and 64-page build/link
  validation pass (`/tmp/session-json-web.log`). The initial `check` script name
  was corrected to the package's `typecheck` script; it was not a source failure.
- Fresh medium independent combined review and its final-delta follow-up report
  no actionable correctness/simplicity findings. Admission received an independent
  preimplementation boundary critique. Final affected race results passed after
  the review; no implementation edits followed the reviewed source.

Native persistence limit discovered during this slice: a non-inference probe of
pinned Codex 0.133.0 accepted thread/start, but thread/resume in a new process failed
with no rollout found (`/tmp/session-json-codex-probe.log`). No turn/start, model,
login or real interface message was requested. The disposable state/processes were
cleaned up. The sample must not claim an ID proves persisted history and propagates
resume failure instead of silently creating another conversation. Review found no
additional Helmr primitive justified by this provider boundary.

The complete real-provider conversation, provider crash durability, VM/process-tree
exclusion and Workspace restore remain qualification gates from the parent plan.
These local checks do not close those gates, authorize paid inference or establish
real Slack delivery. No shared database reset, push, merge or deployment occurred.

## Native process qualification follow-up (2026-09-20)

On candidate `df3d8ec6`, the actual pinned Codex app-server 0.133.0 accepts a
question tool but returns `request_user_input is unavailable in Default mode`
without a client question callback. Experimental client initialization alone is
insufficient. The sample now sets the native
`features.default_mode_request_user_input` configuration on start/resume; a native
probe verifies the resulting callback and answer. This is application-owned
provider configuration, with no Helmr contract or compatibility change.

`dev/workflows/probes/codex-conversation.test.ts` invokes the actual Actor with
fixture Helmr handler/check boundaries and the real pinned subprocess, against a
loopback Responses server. Three cases pass with 36 assertions:

- Question publication and application reply become the expected native answer;
  the answered choice and earlier user/assistant history reach the next model
  request after a fresh native process resumes the same saved thread.
- Aborting during a native question closes the native process, rejects a late
  application reply and allows a fresh process to resume the same native identity
  with the previous user input. This is local Actor cancellation, not physical
  whole-Run stop/hold or power-loss durability.
- A native escalated command approval reaches the application's human request
  flow. Denial reaches the model as a rejected tool result; no approved command
  execution or external side effect is claimed.

Command: `nix develop --command bun test
dev/workflows/probes/codex-conversation.test.ts`; result:
`/tmp/session-codex-conversation-final.log`. Workflow typecheck also passes in
`/tmp/session-codex-conversation.log`. The local model name uses fallback native
metadata; no inference quality or production model configuration is tested.
The probe removes its temporary provider state and closes its owned processes and
loopback listener. Empty-thread resume still fails: starting a real native Turn,
even with fixture model responses, is a materially different persistence boundary.

The host is Darwin arm64. Cloud's checked-in `helmr.rev` is
`e60e4efcb43c5f12ae797635ee9f6f92b97956c0`, not this candidate; no matching
VM validation manifest has been identified. No Cloud deployment was changed.
Real Helmr FIFO/hold/Workspace restore, actual provider inference and Claude native
interaction remain open. These probes do not close package 5.

### Upstream integration and additional native evidence

Product candidate `78ac9d881dbafe9a2b37da060e1c91bee10e610c` merges the
reviewed native sample cut with upstream `64ad957a`. Independent review confirms
that the three overlapping DB/model/migration files contain the upstream additions
without dropping lifecycle changes; all other lifecycle code is unchanged.
DB/schema, full controlplane, Session, telemetry and deployment tests pass in
`/tmp/session-current-integration.log` (controlplane 224.956s).

The paired Cloud validation checkout at `c5deca8f5e8797634028ab82f41c8bff91ab0a13`
pins that exact Product and updates its path report from the removed
completed_actor_record_id to completed_turn_id and the Turn binding fields.
The real PostgreSQL report contract and full Cloud checks pass
(`/tmp/session-cloud-final-checks.log`, exit 0). The initial old-Cloud checks failed
on obsolete module/schema references; those failures were not product regressions.
Fresh read-only review found no actionable integration or report-query findings.

`dev/workflows/probes/claude-conversation.ts` additionally qualifies the actual
pinned Claude SDK/native process using a loopback Messages fixture. Questions
receive the selected answer, a native Bash permission callback is denied (and its
marker file remains absent), and a fresh process with the same native ID sends
the earlier input, assistant output, answered choice and denial back to the model
fixture. Child processes exit before state cleanup. Only fixture authentication
and the minimal required host environment reach the child.
The probe's explicit TypeScript check and native run pass in
`/tmp/session-claude-native-final.log`; fresh medium read-only review found no
actionable findings. It does not exercise the production Claude Actor, its
HumanRequests helper, managed delivery/hold/Workspace restore, actual inference
or crash durability. Provider-native interaction is now tested; integrated Claude
Actor qualification remains open. This follow-up adds only probes/documentation;
AWS build/deployment source remains the clean candidate `78ac9d88`.


### Production Claude Actor qualification

The direct SDK probe has been replaced by
`dev/workflows/probes/claude-conversation.test.ts`. The actual production Actor,
HumanRequests helper, pinned SDK and native processes now run together against a
loopback model fixture. Helmr handler delivery and repository checks remain fixtures.
The isolated process environment contains fixture auth and a temporary home only.

Both scenarios pass (54 assertions, `/tmp/session-claude-actor-test.log`), and the
explicit probe TypeScript check passes (`/tmp/session-claude-actor-types.log`). The
ordinary interaction scenario answers a real native question, rejects a duplicate,
denies a Bash operation without its marker being written, serializes two follow-ups,
and proves the selected answer, denial and messages survive in the same native ID
across fresh Actor invocations. The interrupted scenario rejects the old question's
reply through both old and new message handlers, skips checks and completion for
the interrupted invocation, and continues the same saved conversation.

The initial probe incorrectly expected interruption always to prevent a native
result. The real provider can emit a result during cancellation. The check fixture
was corrected to honor the supplied AbortSignal, like the real repository command;
completion and check counts now prove that result does not settle the interrupted
Helmr Turn. No runtime, SDK or sample behavior change was required. This closes the
previous production-Claude-Actor local integration gap, not VM/hold/restore proof.

## Temporary AWS qualification attempt (2026-09-20)

The paired Product `78ac9d881dbafe9a2b37da060e1c91bee10e610c` and Cloud
`c5deca8f5e8797634028ab82f41c8bff91ab0a13` produced matching native deployment
and Schedule bundles and a verified private Worker AMI. This candidate predates
the later native-probe/sample follow-ups; those remain separately qualified above.

The disposable dev foundation apply stopped at `VpcLimitExceeded` in `us-east-1`:
the account limit is 5 VPCs, with 4 already used before this two-VPC environment.
No Product Run or deployed Session was exercised. This is an environment-capacity
blocker, not evidence of either a passing or failing Session lifecycle.

The manifest-bound destroy passed and verified zero live dev compute/service
resources; 64 partial resources were destroyed. The separate image root's 11
resources, AMI, snapshot, dedicated certificate and validation DNS record were
also removed, with independent state/provider checks. Shared content-addressed
build artifacts remain. HQ's current lifecycle-plan Handoff owns exact cleanup
receipts and the decision needed before another environment attempt. VM delivery,
hold/resume, physical stop and restored native history remain unproved.

## Local native runtime and Control Plane integration (2026-09-20)

On base `bdc2a72c`, `TestSessionNativeLocalPostgres` connects the actual Node
TypeScript `runProgram`, production Codex/Claude Actors and native processes to
real PostgreSQL-backed Control Plane handlers. External input uses the public
TypeScript HTTP client; the interface consumes persisted Session events. No
production runtime, SDK or sample API changed. Shared loopback model fixtures
were extracted from the existing native probes.

Both provider scenarios pass (`/tmp/session-native-local-final.log`, Go 4.981s):

- `send` while idle creates the first Turn. A message submitted after durable
  activation but before runtime delivery/`onMessage` registration is accepted and
  later explicitly rejected by application validation.
- A native question is published through durable output, answered through public
  `send`, delivered by the actual runtime and acknowledged in Postgres. The saved
  native conversation contains the selected answer after a fresh provider process.
- `enqueue` while the question is pending creates a distinct queued Turn. Both
  Turns finish in FIFO order with the same saved native conversation identity.
- Repeating the already answered question with a new message identity is rejected
  by the application. Native command approval is denied; the marker file is absent
  and denial appears in the next provider request. Each provider leaves exactly two
  handled messages and two rejected messages, with no unknown message or failed Turn.
- The sample executes its actual `npm test -- --runInBand` repository check against
  a disposable deterministic test repository before each explicit settlement.
  A late exact-Turn reply rejects after completion.

Run from the Product root, after installing the root and `dev/workflows` pinned
workspace dependencies:

```sh
nix develop --command env HELMR_NATIVE_SESSION_TEST=1 go test ./internal/controlplane -run '^TestSessionNativeLocalPostgres$' -count=1 -v
nix develop --command sh -c 'cd runtime/typescript && bunx tsc --ignoreConfig --noEmit --target esnext --module esnext --moduleResolution bundler --skipLibCheck --types node probes/session-native-local.test.ts src/node-crypto.d.ts'
```

The native qualification is explicit opt-in; enabled runs fail when required
local tools are missing or Postgres skipping was requested. The Go fixture starts
an isolated database and loopback server and builds a temporary Node probe; native
children receive fixture credentials and an isolated home. Node is required by the
production runtime's `randomUUIDv7`; the initial Bun execution was rejected rather
than patched with a production fallback. An initial test bridge incorrectly attached
an artifact to an unchanged Workspace tree; the real commit validator rejected it.
The fixture now follows the existing changed/unchanged capture contract.

The Worker transport, authentication, placement and Workspace capture are fixtures.
The captured tree contains fixture marker bytes, not the native conversation files.
Native history persists on the local filesystem between fresh provider processes
within one Helmr Run. This proves neither Worker/executor transport nor restoration
of that history across a replaced Helmr Run or VM. Integrated interruption/hold is
not covered by these two scenarios; existing native cancellation and Postgres
stop/hold tests cover their respective boundaries separately. Actual provider
inference, physical whole-Run stop and VM restoration remain unproved. No AWS,
paid inference, external interface delivery, main merge or publication occurred.

The affected runtime/probe typechecks pass in
`/tmp/session-native-local-types-final.log`. Existing native probes pass again:
Claude 2 tests / 54 assertions and Codex 3 tests / 36 assertions
(`/tmp/session-native-regression.log`). Focused checkpoint-frontier,
cooperative/between-Turn interruption, changed-hold rejection, HTTP FIFO/exact hold,
and stop-versus-settlement Postgres tests pass in 26.856s
(`/tmp/session-local-lifecycle-regression.log`). Fresh medium read-only combined
review found no actionable correctness or simplicity findings. No SDK redesign
proposal arose from this local evidence.
