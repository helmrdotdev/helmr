# Scripts

Repository maintenance and Product artifact helpers live here. Reusable
Product behavior belongs in Go packages, generated code, or image definitions;
scripts only orchestrate those sources.

## CI parity

Run the full pinned local suite with:

```sh
nix run .#ci-checks
```

Use the narrower `ci-*` Nix apps while iterating. Linux Firecracker execution
still requires a real KVM host and is not emulated in hosted CI.

## Product release artifacts

`scripts/aws-release-artifacts.sh` builds and publishes the Product-owned
Platform release, Control image, and Worker AMIs from a clean exact commit. Its
local receipts live under `.helmr-release-artifacts/` by default.

`scripts/build-control-image.sh` builds a source-only image containing
`helmr-control` and `helmr-dispatcher`. It intentionally excludes deployment
capacity commands and provider policy.

`scripts/aws-bootstrap-helmr-secrets.sh` populates the empty Secrets Manager
containers emitted by generic self-host AWS compositions. It writes values
directly so Terraform state contains only secret ARNs.

Managed Cloud environment operations, provider credentials, root stacks,
capacity automation, and disposable AWS validation live in the private
deployment repository rather than this Product repository.
