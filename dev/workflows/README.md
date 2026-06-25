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
| Active stream transport | `staging` | `active-stream-smoke` |
| Token UX and approval state | `staging` or `production` | `runtime-smoke` with `exerciseToken=true`; `token-checkpoint-smoke` for restore-boundary coverage |
| Timer parked wait resume | `staging` | `timer-smoke` |
| Missing-secret, invalid-payload, and failed-run observability | `staging` | `missing-secret-smoke` request expected to be rejected; malformed payload to `runtime-smoke`; `edge-smoke` expected-error |
| CLI, API, and console inspection | both | `helmr run get`, `helmr run events`, `helmr run logs`, and the console session/task views |

## Deploy & Run

```sh
helmr deploy ./dev/workflows --project helmr --env staging
helmr deploy ./dev/workflows --project helmr --env production

helmr task start runtime-smoke \
  --project helmr \
  --env staging \
  --payload-json '{"scenario":"staging-runtime","expectedEnvironment":"staging"}'

helmr task start secret-smoke \
  --project helmr \
  --env production \
  --payload-json '{"scenario":"production-secrets","expectedEnvironment":"production"}'

helmr task start edge-smoke \
  --project helmr \
  --env staging \
  --payload-json '{"mode":"workspace-overwrite"}'

helmr task start agent-toolchain-smoke \
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

The active stream smoke must complete without creating `run_waits`; verify that
with a DB query when running against AWS dev.

For token UX checks, start a run and complete the pending token from the
console or a trusted bridge:

```sh
helmr task start runtime-smoke \
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
helmr task start missing-secret-smoke --project helmr --env staging

# Strict payload observability: expected to create a failed run with a validation
# error from the task adapter.
helmr task start runtime-smoke \
  --project helmr \
  --env staging \
  --payload-json '{"scenario":"bad-payload","unknown":true}'

# Runtime expected-error observability: expected to fail inside task code.
helmr task start edge-smoke \
  --project helmr \
  --env staging \
  --payload-json '{"mode":"expected-error"}'
```

## Stream Catalogs

Session streams are deployed catalog entries, not ad hoc runtime names. Define
input/output stream handles at module scope and list the handles in the task's
`streams` field:

```ts
const input = streams.input("input-smoke", { schema: inputSchema })
const report = streams.output("input-smoke.report", { schema: reportSchema })

export const smoke = task({
  id: "smoke",
  sandbox: sbx,
  streams: [input, report],
  run: async () => {
    await report.append({ ok: true })
  },
})
```
