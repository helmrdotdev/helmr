# Release validation gate

Helmr release validation is layered so deterministic contract failures stop
before an AWS campaign creates resources:

1. local type checks, unit and integration tests, sample checks, and packed SDK
   consumer validation;
2. `check-pre-aws.sh`, which reports missing required product contracts as
   machine-readable blockers;
3. authenticated public-client and workflow smoke against a deployed control
   plane;
4. the bounded AWS/Firecracker validation campaign and its retained evidence.

Run the product readiness check from the repository root:

```sh
dev/release-gate/check-pre-aws.sh
```

The output uses `helmrdotdev.pre-aws-release-gate.v1`. A blocked result means
the repository is not ready to start the AWS campaign. Implemented preparation
is accepted only through executable tests, type checks, or a packed-package
consumer. Source strings and the disappearance of an old stub are never
readiness evidence.

Go test selections are executed through `run-go-tests.sh`. The gate requires
at least one matching passed test in every selected package, so a renamed or
deleted test cannot become passing evidence merely because `go test -run`
exits successfully.

Passing this gate is readiness, not release certification. In particular, the
local network check proves the named nftables counter and evidence producer
contract. The AWS case must still attribute a live Run to its exact network
namespace and retain a positive `run_denied` counter delta. An arbitrary
connection failure is not accepted as denial evidence.

The AWS manifest must match
`dev/aws/release-validation-profile.json` exactly, including case order,
producer path, check IDs, repetitions, and per-case timeout. `provider-loss` is
last and additionally requires `HELMR_ALLOW_PROVIDER_LOSS=1`; setting it is an
explicit approval for terminating only the manifest-attributed worker instance.
Without that approval, the case fails before mutation.

Every producer that creates a temporary Workspace verifies deletion before it
can emit passing evidence. The campaign owns the sampler process and bounds
each producer. A producer timeout, cleanup failure, or worker sampling failure
therefore fails the case and leaves the campaign in its cleanup path.

Provider credentials and external agent toolchains are certification checks.
They may run after the core product campaign, but they do not replace the
deterministic, authenticated, or Firecracker layers.
