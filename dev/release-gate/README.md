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

The output uses `helmrdotdev.pre-aws-release-gate.v1`. A blocked result is a
release failure, not an optional skip. Implemented preparation is accepted only
through executable tests, type checks, or a packed-package consumer. Known
product gaps remain explicit blockers until each has a positive executable
contract; source strings and the disappearance of an old stub are never
readiness evidence.

The network workflow proves public IPv4 access and the absence of an IPv6
default route. Private-range and metadata denial require a known-reachable
private endpoint or a retained host nftables deny-counter observation. An
arbitrary connection failure is not accepted as denial evidence, so that AWS
observation remains an explicit blocker.

The final AWS manifest must also use a frozen v0 profile with exact case and
check IDs. Category-only coverage remains blocked because substituting a
different case in the same category could otherwise hide a missing contract.

Provider credentials and external agent toolchains are certification checks.
They may run after the core product campaign, but they do not replace the
deterministic, authenticated, or Firecracker layers.
