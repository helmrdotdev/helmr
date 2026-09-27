# Behavior verification

Select the smallest real boundary that proves the changed behavior. Tests are
ordinary source files owned with the behavior they exercise, not entries in a
second permanent case registry. Add, change or delete them with the implementation.
A removed or renamed selected case must fail selection rather than silently pass.

## Choosing a boundary

| Claim | Smallest relevant route | What it does not establish |
| --- | --- | --- |
| Pure logic or protocol rules | Focused native Go selection below | Running dependencies or a guest |
| DB/API transitions, claims or drain authentication | Selected test with real PostgreSQL/Redis below | Live Worker/guest or AWS behavior |
| Normal Task execution and guest filesystem results | Dedicated host and `cases/task.ts` | Persistence, Actor continuation or provider lifecycle |
| Checkpoint and same-host restore | Dedicated host and `cases/persistence.ts` | Cross-host recovery or Actor continuation |
| Actor state across Turns and checkpoint restore | Dedicated host and `cases/actor.ts` | Host-loss or cross-host recovery |
| Guest IPv4 metadata denial | Dedicated host and `cases/network.ts` | General isolation, IPv6 or destination-specific packet tracing |
| IAM, managed capacity, rollout or cross-host behavior | Cloud-owned managed validation for the exact claim | Unselected cases or full scope closure |

The dedicated-host Task, same-host persistence and Actor continuation cases
passed on a disposable dev host on 2026-09-27.
Task persistence does not imply Actor coverage; the Actor case separately asserts
Turn continuity and same-host restore. Select or add the actual behavior's case
for host-loss or other claims. Cases are independently editable source, not a fixed all-suite
requirement. An unavailable route is a proof gap, not a reason to call another
boundary equivalent.

For a continuing repair, retain the same environment identity across attempts.
Before updating, match the installed candidate, retained artifact identities and
data generation to the intended candidate, and inspect any incomplete update.
Service health alone does not establish those identities. Follow the
[update and reset contracts](runtime-host.md#candidate-preparation-and-service-updates);
do not make a new environment for every source fix or restart an interrupted
update blindly. Cloud owns provisioning, retained-host stop/start, environment authority and final
destruction. End-of-objective fixture cleanup is separate from stopping the host;
keep normal login/project setup and verified artifacts for the next objective.

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

## Shared Worker installation

The Worker image and direct disposable-host route use one installer:
`infra/aws/modules/worker-image/templates/install-worker-host.sh`. It stays inside
the Terraform module so consumers of that module receive the same installation
source; it has no AWS API dependency. The image template owns OS packages and
artifact transport, then invokes this installer.

Both modes require the host/runtime tar files, their SHA-256 values, their manifest
SHA-256 values, and the source-owned `prepare-root.sh` next to the installer:

```sh
bash infra/aws/modules/worker-image/templates/install-worker-host.sh verify \
  HOST_TAR HOST_SHA256 HOST_MANIFEST_SHA256 \
  RUNTIME_TAR RUNTIME_SHA256 RUNTIME_MANIFEST_SHA256 \
  infra/aws/modules/worker-image/templates/prepare-root.sh
```

`verify` needs Bash, GNU tar/coreutils and jq, but no root, network or cloud access.
It checks private copies of both archives, exact regular-file membership, manifest
identity and payload digests before any system installation. Use the repository's
infrastructure Nix shell when inspecting artifacts on a workstation.

`install` takes the same arguments on a root-owned Linux x86_64 systemd host. It
requires a quiesced, inactive Worker and the OS tools installed by the image
preparation. It installs the binaries/boot artifacts, service and user identities,
and applies the Worker sysctls without starting the Worker. It does not create
storage partitions, allocate NBD devices, enroll a Worker, write credentials or
prove that the host can run a guest. Use the native artifact producers; do not build
a second payload format for testing.

The [dedicated runtime host profile](runtime-host.md) composes actual services and
the installed Worker, with a separate small Task fixture and executable assertion.
Normal authenticated setup, Task execution, same-host persistence and Actor
continuation have passed
on the dedicated dev profile. Separate checks passed CP-only executable replacement,
edited-initial-migration reset with authenticated Task afterward, and guest IPv4
metadata denial. This does not qualify every update or reset path. It requires
real S3, runtime artifacts and normal authentication. The operating agent uses Cloud’s native runbook for explicit environment cleanup;
there is no independent expiry service.

## Evidence and case maintenance

For a selected change, record the affected behavior, chosen tests and their omitted
boundaries alongside the exact source/artifacts and results. Distinguish a passing
case, case-fixture cleanup, and destruction of the scope environment. Preserve
failures and fix their cause; a later retry does not erase earlier evidence.

Before retiring an execution path, verify its needed callers and assertions have
an executable replacement. Compatibility with unused internal commands is not a
reason to retain duplicate code. Keep live installation and real-VM acceptance
separate from local script/archive checks.
