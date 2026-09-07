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
existing separate workflow and setup action.

The aggregate runs the
Firecracker probe only on x86_64 Linux. Firecracker execution still requires a
real KVM host and is not emulated in hosted CI.

## Development console

`nix run .#dev` runs the control plane and Vite console for interactive local
development. Paseo's `preview` service runs the same stack directly from the
Nix development environment, builds the console, and serves it with the control
plane on one port. Preview state is disposable and isolated under the
worktree's `.helmr-dev` directory. On Linux, PostgreSQL and Redis use Unix
sockets and ClickHouse uses a loopback address derived from Paseo's assigned
service port, so multiple worktrees can run concurrently without coordinating
ports. Preview is available on Linux and Apple Silicon macOS.

On x86_64 Linux, run the browser acceptance test with the Playwright and
Chromium versions pinned by Nix:

```sh
nix develop .#browser --command bun run test:browser
```

The command starts and stops its own disposable stack. To validate a service
that Paseo already supervises, set `HELMR_E2E_BASE_URL` to that service's
localhost URL; the browser test then reuses it instead of starting another
stack. Self-contained runs use `PASEO_PORT` when Paseo supplies it, or port
`4173` otherwise. Set a distinct `HELMR_E2E_PORT` for concurrent
self-contained runs outside Paseo. The same allocated port also isolates the
Linux ClickHouse listener.

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
