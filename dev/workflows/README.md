# Helmr Dev Workflows

This task project contains reusable Helmr product diagnostics. Keep these
workflows focused on behavior a release candidate must prove in both
self-hosted and managed-cloud deployments: task authoring, image builds,
sandboxed execution, payload validation, declared secrets, writable workspaces,
logs, events, streams, tokens, and agent SDK integration.

Company operating workflows live in `../../../company/automation`, not in this
product repo.

## Tasks

| Task | Purpose |
|------|---------|
| `runtime-smoke` | Broad runtime smoke covering run context, workspace writes, source bundling, command execution, stream output, metadata, logs, and optional token waits. |
| `secret-smoke` | Secret-injection smoke for environments that intentionally contain provider/API credentials. Returns only presence, never secret values. |
| `missing-secret-smoke` | Deterministic negative secret-resolution smoke. It intentionally declares a required absent secret and must be rejected before run creation. |
| `edge-smoke` | Focused edge diagnostics for concurrent wait rejection, workspace overwrite behavior, and intentionally failed runs. Missing-secret and invalid-payload cases are external CLI/API assertions because they fail before task code runs. |
| `agent-toolchain-smoke` | Validates the task image, Nix, GitHub access, Claude/Codex/Cursor SDKs, and namespace/runtime assumptions. |
| `stream-input-smoke` | Parks on session input streams, resumes from CLI input sends, and verifies checkpoint-restored workspace/process state. |
| `session-continuation-smoke` | Verifies session-first continuation semantics: the first run returns with the session open/idle, then an idle input append creates a continuation run without another start call. |
| `active-stream-smoke` | Exercises active input stream `once`, `peek`, and `on` without parking or runtime checkpoints. |
| `token-checkpoint-smoke` | Exercises operator tokens across checkpoint restore boundaries. |
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
| Stream input and parked wait resume | `staging` | `stream-input-smoke` |
| Idle session continuation | `staging` | `session-continuation-smoke` |
| Active stream transport | `staging` | `active-stream-smoke` |
| Token UX and approval state | `staging` or `production` | `runtime-smoke` with `exerciseToken=true`; `token-checkpoint-smoke` for restore-boundary coverage |
| Timer parked wait resume | `staging` | `timer-smoke` |
| Missing-secret, invalid-payload, and failed-run observability | `staging` | `missing-secret-smoke` request expected to be rejected; malformed payload to `runtime-smoke`; `edge-smoke` expected-error |
| CLI, API, and console inspection | both | `helmr run get`, `helmr run events`, `helmr run logs`, and the console session/task views |

## Deploy & Run

```sh
helmr deploy ./dev/workflows --project helmr --env staging
helmr deploy ./dev/workflows --project helmr --env production

helmr session start runtime-smoke \
  --project helmr \
  --env staging \
  --payload-json '{"scenario":"staging-runtime","expectedEnvironment":"staging"}'

helmr session start secret-smoke \
  --project helmr \
  --env production \
  --payload-json '{"scenario":"production-secrets","expectedEnvironment":"production"}'

helmr session start edge-smoke \
  --project helmr \
  --env staging \
  --payload-json '{"mode":"workspace-overwrite"}'

helmr session start agent-toolchain-smoke \
  --project helmr \
  --env production \
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

To iterate only on active stream transport, run:

```sh
HELMR_API_URL=https://dev.helmr.dev \
SKIP_DEPLOY=1 \
dev/workflows/scripts/run-active-stream-smoke.sh
```

To iterate only on session continuation, run:

```sh
HELMR_API_URL=https://dev.helmr.dev \
SMOKE_CASES=session-continuation \
SKIP_DEPLOY=1 \
dev/workflows/scripts/run-release-smoke.sh
```

Use comma-separated `SMOKE_CASES` entries to run multiple focused real-usecase
checks, for example `SMOKE_CASES=active-stream,stream-input,token-checkpoint`.
Leave `SMOKE_CASES` unset for the full release smoke.

Before interpreting AWS dev latency numbers, run the measurement preflight:

```sh
AWS_PROFILE=helmr-dev HELMR_MEASUREMENT_PREFLIGHT_ALLOW_ECS_TASK=1 \
nix develop .#infra -c dev/aws/run-measurement-preflight.sh
```

Run it again with `--require-deployments` after the first deploy and before
using `SKIP_DEPLOY=1` for focused measurements. The `LABEL` passed to
`dev/aws/run-smoke-with-path-report.sh` is only an output directory label such
as `hot-60s`; `SMOKE_CASES` must use the script selectors such as `runtime`,
`active-stream`, `stream-input`, `token-checkpoint`, and `timer`.
For latency measurements, set `HELMR_PATH_REPORT_REQUIRE_RUNS=1` on the wrapper
so a smoke that accidentally creates no runs is rejected before analysis. Leave
it unset for smoke cases such as `missing-secrets` that are expected to pass
before run creation. Strict latency measurements also capture sanitized
pre/post surface attestation files in the same report directory, including the
control/dispatcher ECS task definition revision, digest-pinned control image,
current deployment, sandbox ABI/digests, observed runtime identities, and worker
heartbeat/capacity evidence. This keeps wall-clock results tied to the actual
runtime surface that produced them. Interaction smokes also emit `ux_timing`
records for user-visible boundaries such as start returned, stream phase
visible, input accepted, token visible, token completion accepted, and terminal
observed; the wrapper extracts them into `ux-timing.log`.

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

For interaction latency checks, keep the same smoke case and vary only the
human/input delay knobs:

```sh
SMOKE_CASES=token-checkpoint \
TOKEN_CHECKPOINT_DECISION_DELAY_SECONDS=60 \
TOKEN_CHECKPOINT_REPLY_DELAY_SECONDS=60 \
SKIP_DEPLOY=1 \
dev/workflows/scripts/run-release-smoke.sh

SMOKE_CASES=stream-input \
STREAM_INPUT_APPROVAL_DELAY_SECONDS=60 \
STREAM_INPUT_MESSAGE_DELAY_SECONDS=60 \
SKIP_DEPLOY=1 \
dev/workflows/scripts/run-release-smoke.sh
```

The active stream smoke must complete without creating `run_waits`; verify that
with a DB query when running against AWS dev.

For token UX checks, start a run and complete the pending token from the
console or a trusted bridge:

```sh
helmr session start runtime-smoke \
  --project helmr \
  --env staging \
  --payload-json '{"scenario":"token-ui","exerciseToken":true,"tokenTimeout":300}'
```

Some smoke cases intentionally fail before task code runs and therefore do not
produce run records. Treat those as passing only when the CLI/API rejects the
request clearly and no secret values are exposed. Payload schema failures and
other runtime failures should produce `helmr run get`, `helmr run events`, `helmr run logs`,
and console evidence:

```sh
# Missing-secret observability: expected to fail before run creation because
# this task intentionally declares a required absent smoke secret.
helmr session start missing-secret-smoke --project helmr --env staging

# Strict payload observability: expected to create a failed run with a validation
# error from the task adapter.
helmr session start runtime-smoke \
  --project helmr \
  --env staging \
  --payload-json '{"scenario":"bad-payload","unknown":true}'

# Runtime expected-error observability: expected to fail inside task code.
helmr session start edge-smoke \
  --project helmr \
  --env staging \
  --payload-json '{"mode":"expected-error"}'
```

## Streams

Session streams are deployment-level module primitives. Define input/output
stream handles at module scope and use them from task execution. Do not list
streams in `task(...)`; optional stream schemas are runtime validation hints on
the `streams` primitive, not task config.

```ts
const input = streams.input("input-smoke", { schema: inputSchema })
const report = streams.output("input-smoke.report", { schema: reportSchema })

export const smoke = task({
  id: "smoke",
  sandbox: sbx,
  run: async () => {
    await report.append({ ok: true })
  },
})
```
