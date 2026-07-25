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
| Missing-secret, invalid-payload, and failed-run observability | `staging` | `missing-secret-smoke` request expected to be rejected; malformed payload to `runtime-smoke`; `edge-smoke` expected-error |
| CLI, API, and console inspection | both | `helmr run get`, `helmr run events`, `helmr run logs`, and the console Run/Task views |

## Deploy & Run

```sh
helmr deploy ./dev/workflows --project helmr --env staging
helmr deploy ./dev/workflows --project helmr --env production

RUNTIME_WORKSPACE="$(helmr workspace create helmr-runtime-smoke \
  --project helmr --env staging --key release-smoke-runtime \
  --idempotency-key release-smoke-runtime-create)"

helmr run start runtime-smoke \
  --project helmr \
  --env staging \
  --workspace "${RUNTIME_WORKSPACE}" \
  --payload-json '{"scenario":"staging-runtime","expectedEnvironment":"staging"}'

SECRET_WORKSPACE="$(helmr workspace create helmr-secret-smoke \
  --project helmr --env production --key release-smoke-secrets \
  --idempotency-key release-smoke-secrets-create)"

helmr run start secret-smoke \
  --project helmr \
  --env production \
  --workspace "${SECRET_WORKSPACE}" \
  --payload-json '{"scenario":"production-secrets","expectedEnvironment":"production"}'

EDGE_WORKSPACE="$(helmr workspace create helmr-edge-smoke \
  --project helmr --env staging --key release-smoke-edge \
  --idempotency-key release-smoke-edge-create)"

helmr run start edge-smoke \
  --project helmr \
  --env staging \
  --workspace "${EDGE_WORKSPACE}" \
  --payload-json '{"mode":"workspace-overwrite"}'

AGENT_WORKSPACE="$(helmr workspace create helmr-agent-toolchain-smoke \
  --project helmr --env production --key release-smoke-agent \
  --idempotency-key release-smoke-agent-create)"

helmr run start agent-toolchain-smoke \
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
checks, for example `SMOKE_CASES=runtime,token,timer`. Leave `SMOKE_CASES` unset for
the full release smoke.

Before interpreting AWS dev latency numbers, run the measurement preflight:

```sh
AWS_PROFILE=helmr-dev HELMR_MEASUREMENT_PREFLIGHT_ALLOW_ECS_TASK=1 \
nix develop .#infra -c dev/aws/run-measurement-preflight.sh
```

Run it again with `--require-deployments` after the first deploy and before
using `SKIP_DEPLOY=1` for focused measurements. The `LABEL` passed to
`dev/aws/run-smoke-with-path-report.sh` is only an output directory label such
as `hot-60s`; `SMOKE_CASES` must use the script selectors such as `runtime`,
`token`, `timer`, `edge-workspace`, and `production-secrets`.
For latency measurements, set `HELMR_PATH_REPORT_REQUIRE_RUNS=1` on the wrapper
so a smoke that accidentally creates no runs is rejected before analysis. Leave
it unset for smoke cases such as `missing-secrets` that are expected to pass
before run creation. Strict latency measurements also capture sanitized
pre/post surface attestation files in the same report directory, including the
control/dispatcher ECS task definition revision, digest-pinned control image,
current deployment, sandbox ABI/digests, observed runtime identities, and worker
heartbeat/capacity evidence. This keeps wall-clock results tied to the actual
runtime surface that produced them.

After collecting repeated samples, summarize one or more report directories:

```sh
dev/aws/summarize-measurement-reports.sh \
  .helmr-aws-dev-smoke/path-reports/20260629T000000Z-token-hot-60s \
  .helmr-aws-dev-smoke/path-reports/20260629T000100Z-token-hot-60s
```

The summary emits per-report metadata, per-run runtime path classification,
checkpoint artifact role size/encrypt/store timing, per UX timing delta, and
aggregate count/min/p50/p95/max by case, metric, and detail. Use that output
instead of a single wall-clock number when deciding whether an optimization
improved the user experience.

For token UX checks, start a run and complete the pending token from the
console or a trusted bridge:

```sh
helmr run start runtime-smoke \
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
helmr run start missing-secret-smoke --project helmr --env staging \
  --workspace "${STAGING_SECRET_WORKSPACE}"

# Strict payload observability: expected to create a failed run with a validation
# error from the task adapter.
helmr run start runtime-smoke \
  --project helmr \
  --env staging \
  --workspace "${RUNTIME_WORKSPACE}" \
  --payload-json '{"scenario":"bad-payload","unknown":true}'

# Runtime expected-error observability: expected to fail inside task code.
helmr run start edge-smoke \
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
