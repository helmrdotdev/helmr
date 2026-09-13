# Console

Console dashboard for managing a self-hosted Helmr control plane.

## Local development

Run the dashboard against a local control server and managed Postgres/Redis/ClickHouse:

```sh
make dev
```

Open `http://127.0.0.1:<console-port>/dev/login` to create a local owner session. The stack
prints the state directory, socket directory, URLs, and derived ports on startup.

`make dev` starts managed dependencies when `DATABASE_URL`, `REDIS_URL`, and `CLICKHOUSE_URL`
are unset. A synthetic runtime descriptor is written under the dev state directory; no
image build or real Deployment build is required. Live mode runs Vite with hot reload on the
console port and the dev control plane on a separate loopback port (`HELMR_DEV_BACKEND_URL`).

Preview mode (`HELMR_DEV_CONSOLE_MODE=preview`) builds the console once and serves it from
the control plane on a single port (used by browser acceptance tests).

### Worktree isolation

Each worktree uses `$ROOT/.helmr-dev` by default (override with `HELMR_DEV_DIR`). Unix sockets
live under `$TMPDIR/helmr-<state-hash>` (hash of the resolved state directory) for short paths on
macOS. Default loopback ports are derived from that state path; override with `HELMR_DEV_CONSOLE_PORT`,
`HELMR_DEV_CONTROL_PLANE_PORT`, `HELMR_DEV_POSTGRES_PORT`, and `HELMR_DEV_CLICKHOUSE_HTTP_PORT`.
The script fails fast if a required port is already in use or an owned stack is already running.

### Persistence and reset

Owned Postgres, ClickHouse, and CAS data persist across restarts. Dev seed runs once on a fresh
owned database only; renames, deletions, and completed Tokens survive restarts. To restore
fixtures:

```sh
make dev-reset
make dev
```

External `DATABASE_URL` values are never reset. Set `HELMR_DEV_SEED_DATA=false` to skip seeding
on fresh initialization.

### Demo environment fixtures

Production and Staging are empty by default (CLI onboarding on Overview). A third **Demo**
environment is seeded with synthetic Tasks, Actors, Sessions, Runs, Tokens, and related records.
Select **Demo** under Settings → Environments to browse fixtures.

Fixtures are terminal-only: no queued Runs and no live schedule claims. The sample schedule is
archived (no `next_fire_at`; fixture-only, never executed). Pending Tokens use a long `expires_at` so the Demo
Overview stays useful across restarts. Session conversation history is synthetic; submitting new
session input through the console can create real work against your dev stack.
