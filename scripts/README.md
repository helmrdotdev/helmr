# Scripts

Repository maintenance and Product artifact helpers live here. Reusable
Product behavior belongs in Go packages, generated code, or image definitions;
scripts only orchestrate those sources.

## CI parity

Run the repository lane aggregate for the current platform with:

```sh
nix run .#ci-checks
```

For full GitHub CI parity, also run the flake and bundle-builder checks:

```sh
nix flake check --show-trace
nix develop .#images --command bash tests/bundle_builder_e2e.sh
```

Use the narrower `ci-*` Nix apps while iterating. The aggregate runs the
Firecracker probe only on x86_64 Linux. Firecracker execution still requires a
real KVM host and is not emulated in hosted CI.

## Product release artifacts

`scripts/aws-release-artifacts.sh` builds and publishes the Product-owned
Platform release, Control Plane image, and Worker AMIs from a clean exact commit. Its
local receipts live under `.helmr-release-artifacts/` by default.

`scripts/build-controlplane-image.sh` builds a source-only image containing
`control-plane` and `dispatcher`. It intentionally excludes deployment
capacity commands and provider policy.

`scripts/aws-bootstrap-helmr-secrets.sh` populates the empty Secrets Manager
containers emitted by generic self-host AWS compositions. It writes values
directly so Terraform state contains only secret ARNs. It initializes missing
values only and never replaces an existing value.
