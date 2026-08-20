# Helmr Dev Workflows

This task project contains reusable Helmr product diagnostics. Keep these
workflows focused on behavior a release candidate must prove in both
self-hosted and managed-cloud deployments: task authoring, image builds,
sandboxed execution, payload validation, declared secrets, writable workspaces,
logs, events, tokens, and agent SDK integration.

Company operating workflows live in `../../../company/automation`, not in this
product repo.

## Tasks

| Task | Purpose |
|------|---------|
| `runtime-smoke` | Broad runtime smoke covering run context, workspace writes, source bundling, command execution, output, metadata, logs, and optional token waits. |
| `secret-smoke` | Secret-injection smoke for environments that intentionally contain provider/API credentials. Returns only presence, never secret values. |
| `missing-secret-smoke` | Deterministic negative secret-resolution smoke. It intentionally declares a required absent secret and must be rejected before run creation. |
| `edge-smoke` | Focused edge diagnostics for concurrent wait rejection, workspace overwrite behavior, and intentionally failed runs. Missing-secret and invalid-payload cases are external CLI/API assertions because they fail before task code runs. |
| `agent-toolchain-smoke` | Validates the task image, Nix, GitHub access, Claude/Codex/Cursor SDKs, and namespace/runtime assumptions. |
| `timer-smoke` | Parks on a wall-clock timer and verifies workspace state survives resume without active sleep. |
| `child-task-smoke` | Exercises detached `task.start()`, durable `task.call()` success, and a child failure returned as `TaskResult`. |
| `child-task-smoke-actor` | Exercises `task.call()` from successive Actor turns, durable input continuation, and ordered durable Actor output. |
| `network-smoke` | Proves public IPv4 egress while metadata, private IPv4, IPv6 default routing, and public IPv6 are unavailable. |

## Environment Strategy

Use `staging` for control-plane and runtime checks that do not require external
agent credentials. Use `production` for checks that intentionally require
declared API-key secrets or exercise real agent SDKs.

Expected release-smoke coverage:

| Area | Environment | Task |
|------|-------------|------|
| Deploy/build/promotion, source bundle, workspace, logs/events | `staging` | `runtime-smoke` |
| Secret resolution and agent SDK credentials | `production` | `secret-smoke`, then `agent-toolchain-smoke` |
| Token UX and approval state | `staging` or `production` | `runtime-smoke` with `exerciseToken=true` |
| Timer parked wait resume | `staging` | `timer-smoke` |
| Child Task lifecycle | `staging` | `child-task-smoke` through the `child-tasks` release-smoke case; `child-task-smoke-actor` through the authenticated client smoke. |
| Actor continuation and durable output | `staging` | The authenticated client smoke sends two inputs to one Actor, observes a new continuation Run, reads output with pagination, and verifies output remains after close. |
| Declarative Schedule admission and fire | `staging` | The case-owned `dev/schedule-workflows` bundle is promoted only by the dedicated AWS Schedule lifecycle case, then the ordinary execution-only bundle is restored. |
| IPv4-only network boundary | `staging` | `network-smoke` plus the deterministic host-policy test. The guest probe alone is not sufficient evidence. |
| Missing-secret, invalid-payload, and failed-run observability | `staging` | `missing-secret-smoke` request expected to be rejected; malformed payload to `runtime-smoke`; `edge-smoke` expected-error |
| Management resources | `staging` | The authenticated client smoke covers Deployment, Secret, Token, Run list/cancel, and Workspace cleanup through the public SDK. Schedule lifecycle is isolated because it temporarily changes the environment's current deployment. |
| CLI, API, and console inspection | both | `helmr run get`, `helmr run events`, `helmr run logs`, and the console Run/Task views. Missing v0 surfaces are reported by the pre-AWS gate. |

## Deploy & Run

```sh
helmr deploy ./dev/workflows --project helmr --env staging
helmr deploy ./dev/workflows --project helmr --env production

RUNTIME_WORKSPACE="$(helmr workspace create helmr-runtime-smoke \
  --project helmr --env staging --key release-smoke-runtime \
  --idempotency-key release-smoke-runtime-create)"

helmr task start runtime-smoke \
  --project helmr \
  --env staging \
  --workspace "${RUNTIME_WORKSPACE}" \
  --payload-json '{"scenario":"staging-runtime","expectedEnvironment":"staging"}'

SECRET_WORKSPACE="$(helmr workspace create helmr-secret-smoke \
  --project helmr --env production --key release-smoke-secrets \
  --idempotency-key release-smoke-secrets-create)"

helmr task start secret-smoke \
  --project helmr \
  --env production \
  --workspace "${SECRET_WORKSPACE}" \
  --payload-json '{"scenario":"production-secrets","expectedEnvironment":"production"}'

EDGE_WORKSPACE="$(helmr workspace create helmr-edge-smoke \
  --project helmr --env staging --key release-smoke-edge \
  --idempotency-key release-smoke-edge-create)"

helmr task start edge-smoke \
  --project helmr \
  --env staging \
  --workspace "${EDGE_WORKSPACE}" \
  --payload-json '{"mode":"workspace-overwrite"}'

AGENT_WORKSPACE="$(helmr workspace create helmr-agent-toolchain-smoke \
  --project helmr --env production --key release-smoke-agent \
  --idempotency-key release-smoke-agent-create)"

helmr task start agent-toolchain-smoke \
  --project helmr \
  --env production \
  --workspace "${AGENT_WORKSPACE}" \
  --payload-json '{"repository":"helmrdotdev/helmr","ref":"main"}'
```

For a repeatable CLI/API release gate, run the harness from the repository root
after logging in:

```sh
HELMR_API_URL=https://dev.helmr.dev \
dev/workflows/scripts/run-release-smoke.sh
```

Set `HELMR_BIN=/path/to/helmr` to test a prebuilt CLI binary. When unset, the
harness runs `go run ./cmd/helmr` from the repository root. Set `SKIP_DEPLOY=1`
to reuse the currently promoted deployments and run only the smoke cases.

Use comma-separated `SMOKE_CASES` entries to run multiple focused real-usecase
checks, for example
`SMOKE_CASES=runtime,token,timer,child-tasks,concurrent-wait,network`. Leave
`SMOKE_CASES` unset for the full release smoke.

Before creating AWS resources, run the zero-spend product readiness gate:

```sh
dev/release-gate/check-pre-aws.sh
```

The command exits nonzero while a required product contract is still absent.
It does not turn known implementation gaps into skipped or passing smoke cases.

Deployment-specific latency collection, provider attribution, and disposable
AWS evidence are maintained outside the Product repository. These fixtures
remain the Product workload source used by that deployment validation.

For token UX checks, start a run and complete the pending token from the
console or a trusted bridge:

```sh
helmr task start runtime-smoke \
  --project helmr \
  --env staging \
  --workspace "${RUNTIME_WORKSPACE}" \
  --payload-json '{"scenario":"token-ui","exerciseToken":true,"tokenTimeout":300}'
```

The release harness discovers the Token through the Run's typed pending Wait,
not through an application-defined output channel. Set
`TOKEN_DECISION_DELAY_SECONDS` when measuring checkpoint/restore behavior.

Some smoke cases intentionally fail before task code runs and therefore do not
produce run records. Treat those as passing only when the CLI/API rejects the
request clearly and no secret values are exposed. Payload schema failures and
other runtime failures should produce `helmr run get`, `helmr run events`, `helmr run logs`,
and console evidence:

```sh
# Missing-secret observability: expected to fail before run creation because
# this task intentionally declares a required absent smoke secret.
STAGING_SECRET_WORKSPACE="$(helmr workspace create helmr-secret-smoke \
  --project helmr --env staging --key release-smoke-missing-secret \
  --idempotency-key release-smoke-missing-secret-create)"
helmr task start missing-secret-smoke --project helmr --env staging \
  --workspace "${STAGING_SECRET_WORKSPACE}"

# Strict payload observability: expected to create a failed run with a validation
# error from the task adapter.
helmr task start runtime-smoke \
  --project helmr \
  --env staging \
  --workspace "${RUNTIME_WORKSPACE}" \
  --payload-json '{"scenario":"bad-payload","unknown":true}'

# Runtime expected-error observability: expected to fail inside task code.
helmr task start edge-smoke \
  --project helmr \
  --env staging \
  --workspace "${EDGE_WORKSPACE}" \
  --payload-json '{"mode":"expected-error"}'
```

## Durable interaction

Named Run and Session streams are not part of the v0 contract. Stable
interactive execution uses an Actor's fixed durable `input` and `output`
channels. One-shot external approvals use Tokens. Direct Task Runs retain
logs, events, and a terminal result.
