# Scripts

Repository maintenance and Product artifact helpers live here. Reusable
Product behavior belongs in Go packages, generated code, or image definitions;
scripts only orchestrate those sources.

## Source checks and release validation

Every PR and main push runs a fixed source set:

```sh
nix run .#ci-fast-policy
nix run .#ci-fast-go
nix run .#ci-fast-typescript
```

`ci complete` requires all three checks. No path routing, label override or
release artifact construction runs in source CI. Go tests skip the separate
PostgreSQL suite explicitly; they compile the embedded Console once per Go job.
TypeScript tests build required local workspace packages but do not pack or
publish a distribution.

[Release checkpoints](release/README.md) run generated-source, PostgreSQL and
browser validation and the real artifact consumer. For deeper targeted work,
use `ci-go-race`, `ci-clickhouse`, `ci-infra-test`, `ci-bundle-builder`,
`ci-version-cohort`, `ci-boot-artifacts-repro`, or `nix flake check`. These commands
remain available without forcing every source edit through their setup costs.
`ci-policy` is the comprehensive local repository/release-contract check;
`ci-checks` remains the broad local source aggregate.

## Development console

`nix run .#dev` (or `make dev`) runs the Vite console with hot reload and a
managed Postgres, Redis, ClickHouse, and synthetic runtime descriptor when
external URLs are not set. State lives under `$ROOT/.helmr-dev` (override
`HELMR_DEV_DIR`) with short-path Unix sockets in `$TMPDIR/helmr-<state-hash>`.
Owned data persists by default; `make dev-reset` clears owned Postgres, ClickHouse,
and CAS only when the stack is stopped and no owned services are still running.

If startup or reset reports an existing `.stack.lock`, stop the stack (or any
orphaned owned Postgres/Redis/ClickHouse under that state directory), then remove
`.stack.lock` manually. Locks are never reclaimed automatically.

`HELMR_DEV_CONSOLE_MODE=preview` builds the console once and serves it from the
control plane on one loopback port (browser tests). Live mode uses separate
console and control-plane ports; Vite `strictPort` and `HELMR_DEV_BACKEND_URL`
must agree.

Run stack integration checks:

```sh
./scripts/dev-console-stack.test.sh
```

Run browser acceptance tests (single managed preview stack, reset per run):

```sh
bun run test:browser
```

On x86_64 Linux, use the pinned Playwright browsers:

```sh
nix run .#ci-browser
```

To reuse an already-running preview stack with Demo fixtures seeded, set
`HELMR_E2E_BASE_URL`. Managed runs use `$ROOT/.helmr-dev-e2e` and
`HELMR_E2E_PORT` (default `4173`).

## Product release artifacts

`scripts/aws-release-artifacts.sh` manages the release-build foundation and
Product-owned Worker image operations. Its local receipts live under
`.helmr-release-artifacts/` by default.

`scripts/build-controlplane-image.sh` builds a source-only image containing
`control-plane` and `dispatcher`. It intentionally excludes deployment
capacity commands and provider policy. The release workflow publishes that
image and the signed Platform release.

`scripts/aws-bootstrap-helmr-secrets.sh` populates the empty Secrets Manager
containers emitted by generic self-host AWS compositions. It writes values
directly so Terraform state contains only secret ARNs. It initializes missing
values only and never replaces an existing value. Self-hosted AWS uses a dedicated
Computer wrapping key container whose random 32-byte value stays outside Terraform
state. The module input `computer_wrapping_key_id` identifies the root key independently
of its storage container (default: `computer-root-v1`). Preserve this logical ID and
key value with database backups, including when restoring into a new container.
Changing either does not rewrap existing Computer keys. Managed Cloud instead uses the existing KMS key directly, with
Computer context restrictions on the Control Plane task role.

## Testing an unreleased SDK locally

For development, build the SDK and proto package from one clean, recorded Helmr
commit. Use a CLI/compiler and staging runtime from that same source contract;
local tarballs do not provide cross-version compatibility. This workflow does
not publish packages or create a release channel.

In an isolated Helmr checkout, enter `nix develop`, then run:

```sh
bun install --frozen-lockfile --ignore-scripts
git rev-parse HEAD
export PACKAGE_VERSION="0.1.0-dev.$(git rev-parse --short=12 HEAD)"
bash scripts/build-npm-packages.sh
bash scripts/pack-npm-packages.sh
```

Copy the two generated archives from `dist/npm-tarballs/` into the consuming
project's `vendor/` directory. Set both dependencies to their actual filenames:

```json
{
  "dependencies": {
    "@helmr/sdk": "file:vendor/helmr-sdk-0.1.0-dev.COMMIT.tgz",
    "@helmr/proto": "file:vendor/helmr-proto-0.1.0-dev.COMMIT.tgz"
  },
  "overrides": {
    "@helmr/proto": "file:vendor/helmr-proto-0.1.0-dev.COMMIT.tgz"
  }
}
```

Replace `COMMIT` with the recorded revision suffix. The Bun override keeps the
SDK's transitive proto dependency on the same local artifact. Run `bun install --ignore-scripts`
once to update the lockfile; retain the archives, lockfile and full source
revision together. Subsequent checks use `bun install --frozen-lockfile --ignore-scripts`.
Run the application's typecheck/tests and a full program compilation with the
matching compiler. Declaration discovery alone does not exercise dependency
packaging. A local compile does not prove an image build or a deployed runtime.

The package manager owns archive installation and integrity. Helmr retains the
installed tree and automatically transforms reached TypeScript/JSX, including
copied and linked packages, without package selectors or guessed source identity.
Node-ready JavaScript keeps native exports, cache and asset locations. Use a
target-platform install for native addons. See the
[configuration reference](../packages/web/src/content/docs/reference/configuration.md#installed-dependencies-and-source-execution)
for resolution precedence, source containment and image-layout requirements.

Keep `vendor/` in the captured project so the build can install these files.
The archives also remain in the program source tree; account for their size.
