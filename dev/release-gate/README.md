# Release validation gate

Helmr Product release validation is layered so deterministic contract failures
stop before any deployment repository creates provider resources:

1. local type checks, unit and integration tests, sample checks, and packed SDK
   consumer validation;
2. `check-pre-aws.sh`, which reports missing required product contracts as
   machine-readable blockers;
3. authenticated public-client and workflow smoke against a deployed controlplane
   plane.

Run the product readiness check from the repository root:

```sh
dev/release-gate/check-pre-aws.sh
```

The output uses `helmrdotdev.pre-aws-release-gate.v1`. A blocked result means
the Product repository is not ready for deployment validation. Implemented preparation
is accepted only through executable tests, type checks, or a packed-package
consumer. Source strings and the disappearance of an old stub are never
readiness evidence.

Go test selections are executed through `run-go-tests.sh`. The gate requires
at least one matching passed test in every selected package, so a renamed or
deleted test cannot become passing evidence merely because `go test -run`
exits successfully.

Passing this gate is Product readiness, not deployment certification. A
deployment repository consumes the exact Product commit and owns live provider
topology, exact host loss, disposable infrastructure, and retained evidence.

Provider credentials and external agent toolchains are certification checks.
They may run after the core product campaign, but they do not replace the
deterministic, authenticated, or Firecracker layers.
