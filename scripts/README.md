# Scripts

Repository maintenance and Product artifact helpers live here. Reusable
Product behavior belongs in Go packages, generated code, or image definitions;
scripts only orchestrate those sources.

## CI parity

Run the repository lane aggregate for the current platform with:

```sh
nix run .#ci-checks
```

On x86_64 Linux, also run the artifact and browser checks for full GitHub CI parity:

```sh
nix flake check --show-trace
nix run .#ci-bundle-builder
nix run .#ci-version-cohort
nix run .#ci-boot-artifacts-repro
nix run .#ci-browser
```

Use the narrower `ci-*` Nix apps while iterating. Their runtime inputs are
specific to each check; interactive shells retain the full development tools.
The boot reproducibility check requires a clean checkout. CI separates Nix app
preparation from execution so step durations distinguish environment construction
from validation. Go race tests use `-count=1` to execute tests even when compiled
packages are cached. Browser failure screenshots and traces are retained as the
`browser-failure` Actions artifact for seven days.

All checks still run on every PR and main push. The required `ci complete` check
rejects failure, cancellation, or a skipped dependency. Release builds use their
existing separate workflow and setup action. Version-cohort and boot-reproducibility
checks run independently in the release-contract matrix; both must pass.

The Nix flake job pilots a CI-only store cache keyed by OS, architecture,
Nix/Go dependency inputs and commit. A job/OS/architecture prefix can reuse
existing store paths after input changes; Nix still evaluates the current inputs.
The bundle-builder job restores the same cache without saving or waiting for the
current flake job. Both checks run regardless of cache hits. Pull-request caches remain isolated to their merge ref and are
never restored by release builds. Current check outputs are GC roots, and
`keep-outputs` preserves their build dependencies. The 2 GiB pre-save GC target
only removes unrooted paths; the retained closure can exceed it. Measure cold, exact-hit and new-commit prefix-hit runs including
restore and post-job save time before extending the cache to other jobs.

The race job independently pilots a Go compilation/module cache under
`/tmp/helmr-ci-go-{build,mod}` on the disposable Linux runner, keyed by OS,
architecture, pinned toolchain/dependencies and commit. It still runs all race
tests with `-count=1`; cache hits never skip the check. Only one job writes each
cache family. Neither cache changes the repository's storage quota or eviction
policy.

The aggregate runs the
Firecracker probe only on x86_64 Linux. Firecracker execution still requires a
real KVM host and is not emulated in hosted CI.

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
values only and never replaces an existing value.
