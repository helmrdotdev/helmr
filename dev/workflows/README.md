# Helmr Dev Workflows

This task project contains reusable Helmr product diagnostics. Keep these
workflows focused on behavior a release candidate must prove in both
self-hosted and managed-cloud deployments: task authoring, image builds,
sandboxed execution, payload validation, declared secrets, writable workspaces,
logs/events, waitpoints, and agent SDK integration.

Company operating workflows live in `../../../company/automation`, not in this
product repo.

## Tasks

| Task | Purpose |
|------|---------|
| `runtime-smoke` | Broad runtime smoke covering run context, workspace writes, source bundling, command execution, emitted events, logs, and optional waitpoints. |
| `secret-smoke` | Secret-injection smoke for environments that intentionally contain provider/API credentials. Returns only presence, never secret values. |
| `edge-smoke` | Focused edge diagnostics for concurrent wait rejection, workspace overwrite behavior, and intentionally failed runs. Missing-secret and invalid-payload cases are external CLI/API assertions because they fail before task code runs. |
| `agent-toolchain-smoke` | Validates the task image, Nix, GitHub access, Claude/Codex/Cursor SDKs, and namespace/runtime assumptions. |
| `waitpoint-checkpoint-smoke` | Exercises human waitpoints across checkpoint restore boundaries. |

## Environment Strategy

Use `staging` for control-plane and runtime checks that do not require external
agent credentials. Use `production` for checks that intentionally require
declared API-key secrets or exercise real agent SDKs.

Expected release-smoke coverage:

| Area | Environment | Task |
|------|-------------|------|
| Deploy/build/promotion, source bundle, workspace, logs/events | `staging` | `runtime-smoke` |
| Secret resolution and agent SDK credentials | `production` | `secret-smoke`, then `agent-toolchain-smoke` |
| Waitpoint UX and approval state | `staging` or `production` | `runtime-smoke` with `exerciseWaitpoint=true`; `waitpoint-checkpoint-smoke` for restore-boundary coverage |
| Missing-secret, invalid-payload, and failed-run observability | `staging` | `secret-smoke` request expected to be rejected; malformed payload to `runtime-smoke`; `edge-smoke` expected-error |
| CLI, API, and console inspection | both | `helmr show`, `helmr events`, `helmr logs`, and the console run/task views |

## Deploy & Run

```sh
helmr deploy ./dev/workflows --project helmr --env staging
helmr deploy ./dev/workflows --project helmr --env production

helmr run runtime-smoke \
  --project helmr \
  --env staging \
  --payload-json '{"scenario":"staging-runtime","expectedEnvironment":"staging"}'

helmr run secret-smoke \
  --project helmr \
  --env production \
  --payload-json '{"scenario":"production-secrets","expectedEnvironment":"production"}'

helmr run edge-smoke \
  --project helmr \
  --env staging \
  --payload-json '{"mode":"workspace-overwrite"}'

helmr run agent-toolchain-smoke \
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

For waitpoint UX checks, start a run and complete the pending waitpoint from the
console:

```sh
helmr run runtime-smoke \
  --project helmr \
  --env staging \
  --payload-json '{"scenario":"waitpoint-ui","exerciseWaitpoint":true,"waitpointTimeout":300}'
```

Some smoke cases intentionally fail before task code runs and therefore do not
produce run records. Treat those as passing only when the CLI/API rejects the
request clearly and no secret values are exposed. Payload schema failures and
other runtime failures should produce `helmr show`, `helmr events`, `helmr logs`,
and console evidence:

```sh
# Missing-secret observability: expected to fail in staging because staging has
# no provider API secrets.
helmr run secret-smoke --project helmr --env staging

# Strict payload observability: expected to create a failed run with a validation
# error from the task adapter.
helmr run runtime-smoke \
  --project helmr \
  --env staging \
  --payload-json '{"scenario":"bad-payload","unknown":true}'

# Runtime expected-error observability: expected to fail inside task code.
helmr run edge-smoke \
  --project helmr \
  --env staging \
  --payload-json '{"mode":"expected-error"}'
```
