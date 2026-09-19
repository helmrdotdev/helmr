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

## Remaining integration gates

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

## Implemented boundary

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

## Results

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

## Independent review and calibration

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
