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
- `cases/<behavior>/task.ts`: guest Task/Actor/Sandbox definitions needed by that
  behavior. Related cases may use an existing fixture with a relative import.
- `support/`: bounded waits and evidence/cleanup mechanics shared by actual callers.
- `fixtures/schedule/`: intentionally separate scheduled deployment. Promote it
  only for Schedule validation; restore the ordinary bundle afterward.
- `prepare_project.py`: isolate selected fixtures and locally packed SDK dependencies.

Keep assertions next to their stimulus. Split by a separately meaningful claim,
not one file per API call. Do not put deployment, account setup or provider fault
injection into an ordinary behavior case. External agent examples live in
`examples/issue-fixer`, outside the basic runtime project's dependencies.

| Cases | Fixture directories passed to preparation | Claim |
| --- | --- | --- |
| `task` | `cases/task` | Minimal marker round-trip and guest filesystem |
| `persistence` (including `shared.run.ts`) | `cases/persistence` | Single-member and shared-Computer checkpoint and same-host restore |
| `actor` | `cases/actor` | Actor Turns and same-host checkpoint restore |
| `network` | `cases/network` | Guest metadata denial plus exact host packet observation |
| `runtime`, `computer-command`, `program-replacement.run.ts` | `cases/runtime` | Runtime tools/files/logs; Computer idempotency/exec |
| `token-wait`, `token-fanout` | `cases/token-wait` | Internal Token creation/resumption; shared Token fan-out and completion before wait |
| `actor-continuity`, `child-tasks` (including `cancel-peer.run.ts`) | `cases/child-tasks` | Child modes, cancellation without stopping a shared-Computer peer, Actor continuation and ordered/paginated durable output |
| `timer`, `run-cancel` | `cases/timer` | Timer completion or explicit cancellation |
| `network-egress` | `cases/network-egress` | Public IPv4 succeeds, no IPv6 default route |
| `computer-overwrite`, `concurrent-wait`, `invalid-payload`, `expected-error` | `cases/computer-overwrite` | Filesystem overwrite and exact negative contracts |
| `secret-injection`, `missing-secret` | `cases/secret-injection` | Disposable secret value binding or missing-secret admission failure |
| `secrets`, `token-cancel`, `deployment` | None (already initialized scope) | Focused management API contracts |

`computer-durability`, `fault-probe`, and `datapath-network` contain guest fixtures
for provider-owned assertions, not standalone Product case drivers. The Schedule
fixture is likewise consumed by provider validation.

## Prepare and run

```sh
nix develop -c python3 tests/e2e/prepare_project.py /private/case-project \
  --fixtures cases/task
helmr deploy /private/case-project
HELMR_EVIDENCE_DIR=/private/attempt/task \
  bun run /private/case-project/cases/task/run.ts
```

Use normal `HELMR_API_URL` and `HELMR_API_KEY` for the explicitly selected scope.
The preparation command builds/packs the local SDK without npm publication; use
`--sdk-packages DIR` to reuse exact previously packed SDK/proto tarballs. Select
only fixture directories needed by the case. It creates a new outside-checkout
project, excludes case drivers from the deployment, and installs dependencies.
Rerun preparation into a new directory after source/SDK changes.

Host-observing `actor`, `persistence` and `network` cases run on the dedicated host
and require `HELMR_RUNTIME_HOST_TOOL` to name its installed `dev/runtime/host.py`.
Other cases use only the native API and can target an explicitly authorized endpoint.
Different-Computer child calls require at least two available VM slots: a hot
parent can retain its slot while its child runs. Before selecting `child-tasks`,
check the dedicated Worker's advertised capacity. On a sufficiently sized host,
`WORKER_CAPACITY_VCPUS=4`, `WORKER_CAPACITY_MEMORY_MIB=4096` and
`WORKER_EXECUTION_SLOTS=2` allow two default
2-vCPU / 2-GiB VMs; the backing disk and assigned NBD devices must cover both.
Configure this before enrollment, following the host profile's replacement rules.
Cases requiring Secret management or Computer exec need those exact API-key
permissions; do not broaden an existing key merely to run every case.

The evidence directory must be new and its parent must exist. Most focused cases
write `result.json`; Task, persistence, Actor and network retain their detailed
`task.json`, `persistence.json`, `actor.json` and `network.json` receipts. Evidence
contains exact created object IDs, assertions and cleanup outcomes. Accepted delete
requests do not establish storage reclamation or environment retirement. Preserve
failure evidence and diagnose the actual boundary before retrying.

Use the [host runbook](../../dev/runtime/README.md) for matching installed artifacts,
service updates and edited-initial-migration reset. Reuse a healthy host throughout
the repair objective, clean up its test fixtures, then stop compute. Provider
provisioning, retained storage costs and final retirement belong to deployment
operations. A previous passing case does not qualify a changed case or artifact.

## Compatible Program replacement

`cases/runtime/program-replacement.run.ts` retains one Computer across two normal
Deployment promotions. Build the ordinary selected project, then a second isolated
copy changing only `runtimeSmoke`'s returned report to include
`programRevision: "next"`. Verify both bundles have identical Computer images and
Sandbox specifications and different Program identities. Set
`HELMR_NEXT_BUNDLE_DIGEST` to the second bundle digest. Deploy the original bundle
and start the driver. After `ready-for-deploy.json` appears in its evidence
folder, promote the second bundle through the normal CLI; the driver then verifies
new code, the selected deployment and preservation of the first Run's files.
Restore the ordinary deployment after the case. Deployment orchestration stays
outside the driver. Use host evidence to confirm the old instance was physically
excluded before the replacement became ready; SDK results alone do not prove that
boundary.

## Real PostgreSQL and Redis

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
nix develop -c scripts/test-go-selection.sh '^TestProgramResumeGrantPreservesFrozenScope$' ./internal/guestd
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

`cases/computer-restore` supplies a parent and child sharing one Computer for
provider-controlled replacement verification. Both preserve independent memory
nonces and files; the child enters a native token Wait while the parent awaits
its call. A verifier must observe both members in the same ready checkpoint,
source reclamation, the replacement Host and one destination Instance with exact
checkpoint/disk lineage. Completing the child's token allows both to resume;
the parent verifies the child's post-restore file write. Its one-hour Task and
45-minute Wait budgets include bounded host replacement. Deployment owners own
provider retirement, baseline restoration and cleanup; ordinary elapsed time or
Task success does not establish a checkpoint restore.
