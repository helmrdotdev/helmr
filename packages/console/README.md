# Console

Console provides inspection, settings and operational controls for a Helmr control
plane. Start Agents, send Session work, answer questions and manage Computers
through the CLI or a connected Slack conversation. Console retains Session
interrupt/cancel, uncertain Slack delivery recovery, and Deployment list/detail
and Promote with its normal permission and confirmation. It shows complete
question details and exact CLI answer instructions.

## Local development

Run the dashboard against a local control server and managed Postgres/Redis/ClickHouse:

```sh
make dev
```

Open `http://127.0.0.1:<console-port>/dev/login` to create a local owner session. The stack
prints the state directory, socket directory, URLs, and derived ports on startup.

`make dev` starts managed dependencies when `DATABASE_URL`, `REDIS_URL`, and `CLICKHOUSE_URL`
are unset. `CAS_URI` and `PLATFORM_STORE_URI` are required: distinct S3 stores, as described in
[scripts/README.md](../../scripts/README.md#development-console). A synthetic runtime descriptor is written under the dev state directory; no
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

Owned Postgres and ClickHouse data persist across restarts; S3 objects are never reset. Dev seed runs once on a fresh
owned database only; renames, deletions, and retained Session history survive restarts. To restore
fixtures:

```sh
make dev-reset
make dev
```

External `DATABASE_URL` values are never reset. Set `HELMR_DEV_SEED_DATA=false` to skip seeding
on fresh initialization.

### Demo environment fixtures

Production and Staging are empty by default (CLI onboarding on Overview). A third **Demo**
environment contains a synthetic Agent definition, a revoked Deployment, a Computer with
failed preparation, and a closed Session with one failed Turn. Select **Demo** under
Settings → Environments to browse its retained input and events.

These fixtures are illustrative terminal history. They have no executable bundle,
VM, process, Save or checkpoint. The Deployment is revoked and its schedule has ended,
so browsing the fixtures cannot create Worker demand. Deploy a real Agent in an empty
environment to start new work.
