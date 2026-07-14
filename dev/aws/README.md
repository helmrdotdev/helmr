# AWS validation campaigns

`run-validation-campaign.sh` is the formal, resumable gate around the existing
AWS dev scripts. It does not replace `scripts/aws-dev-smoke.sh`,
`scripts/aws-dev-debug.sh`, or the workflow smoke scripts. It freezes their
inputs, records structured stage results, and publishes an encrypted evidence
bundle before the ephemeral stack is destroyed.

The committed campaign manifest is owned by the ops repository. Its strict
schema binds the Helmr commit, the `dev/workflows` Git tree, the harness hash,
the account-independent control repository/digest, worker AMI and instance
type, byte-exact control and worker tfvars, exact case payloads, committed
producer paths and hashes,
retry policy, the 1 run + 1 build worker ceiling, one NAT gateway ceiling, and
a 30-day / 50 MiB evidence budget. A manifest is valid only from a clean
committed ops checkout and a clean matching Helmr checkout.

The fixed stage order is:

1. `preflight`
2. `control_up`
3. `awaiting_human`
4. `auth_ready`
5. `worker_up`
6. `workload`
7. `pre_shutdown_publish`
8. `cleanup`
9. `closed`
10. `post_shutdown_publish`

The human signup and CLI approval pause is deliberately between invocations.
`run-auth-readiness.sh` then proves the exact project and environment slugs
through both the database preflight and an authenticated CLI request without
persisting user, organization, project, or environment identifiers.

Each stage result uses this envelope:

```json
{
  "schema": "helmrdotdev.validation-stage-result.v1",
  "stage": "preflight",
  "status": "passed",
  "reason": null,
  "observations": {},
  "cases": []
}
```

`observations` is not an extensibility bag. The harness enforces a stage-specific
allowlist. Control and worker collectors require healthy ECS rollouts, available
RDS and Valkey, the exact image/AMI and launch templates, live ASG bounds,
lifecycle hooks, scale-in protection, creation times, and the pinned AWS price
fixture. Cleanup verifies the manifest-bound OpenTofu backend and proves zero
OpenTofu state, stack-tagged resources, NAT, workers, RDS, Valkey, ECS, load
balancers, target groups, and VPC endpoints. Unknown keys, arbitrary logs, and
oversized JSON records are rejected.

The workload result must contain every manifest case exactly once. Every
attempt has only an index, terminal status, bounded reason code, producer hash,
and SHA-256 of its source result. The `workload` command is the only writer. It
executes each manifest-bound committed producer, samples both live ASGs, and
reads NAT byte metrics itself. Caller-made
attempt JSON and caller-reported worker peaks are not accepted:

```sh
dev/aws/run-validation-campaign.sh auth MANIFEST
dev/aws/run-validation-campaign.sh workload MANIFEST
```

Typical local setup after both repositories are committed:

```sh
dev/aws/run-validation-campaign.sh validate /absolute/path/to/campaign.json
dev/aws/run-validation-campaign.sh init /absolute/path/to/campaign.json
dev/aws/run-validation-campaign.sh claim /absolute/path/to/campaign.json
dev/aws/run-validation-campaign.sh status /absolute/path/to/campaign.json
```

Use `run ... -- COMMAND` only for the preflight and explicit human pause.
Authenticated readiness is owned by `auth`, and workload execution is owned by
`workload`. Control apply, worker apply, and cleanup are wrapped by
`run-collect`; the wrapper keeps a live owner PID for the whole command and
generates the result itself from the verified S3 backend plus live ECS, EC2,
Auto Scaling, RDS, Valkey, NAT, load-balancer, VPC-endpoint, and tagging reads.
Handwritten results cannot complete those stages. `close` repeats the live
zero-resource inventory before final evidence is published:

```sh
dev/aws/run-validation-campaign.sh run-collect MANIFEST control_up -- COMMAND
dev/aws/run-validation-campaign.sh run-collect MANIFEST worker_up -- COMMAND
dev/aws/run-validation-campaign.sh run-collect MANIFEST cleanup -- COMMAND
dev/aws/run-validation-campaign.sh close MANIFEST
dev/aws/run-validation-campaign.sh publish MANIFEST post-shutdown
```

The generic
wrapper always records a missing result, a non-zero/result conflict, or an
invalid result as a failed stage.
If the process or laptop dies before the trap can run, `recover MANIFEST`
records the interrupted stage as failed. A failed forward stage proceeds to
`pre_shutdown_publish`, then cleanup; teardown never erases the best available
failure evidence first. The campaign verdict is tracked independently from
successful cleanup and closure.
Use `publish ... pre-shutdown` before cleanup and `publish ... post-shutdown`
after the zero-resource inventory has been recorded.

The evidence namespace must be claimed before any spend-increasing stage.
AWS-mutating stages verify region, dev name, state key, and STS account identity.
The control and worker stages hash the complete generated tfvars, so instance,
volume, guest/runtime, RDS, Valkey, ClickHouse, and fleet-controller drift is
rejected before apply without committing private account identifiers. The
worker stage also rejects nonzero
initial desired capacity, ASG/fleet-controller values over 1+1, more than one
NAT gateway, or image/AMI/instance drift. Run and build launch-template IDs and
versions are then captured from AWS. A filesystem lock prevents concurrent
local state transitions; a live stage PID prevents recovery from racing an
active apply or cleanup, and stale owners remain recoverable.

Source or manifest drift stops forward/spend-increasing stages. It never blocks
the pre-shutdown evidence attempt, cleanup, or the post-shutdown publication.
This prevents a validation failure from stranding NAT, database, worker, or
ClickHouse resources.

Evidence is stored under the KMS-encrypted, versioned bootstrap artifact bucket
at `helmr/validation-evidence/<namespace>/`. Bundles are create-only and
verified against the exact returned S3 version, SHA-256 checksum, byte length,
and KMS key. Evidence versions remain for at least 30 days. A sub-KiB permanent
claim at `helmr/validation-claims/<namespace>/claim.json` prevents reuse after
local state loss; it is outside the evidence lifecycle rule. Only strict JSON
records are bundled; unrestricted command logs, environment dumps, CLI tokens,
and `~/.config/helmr` are excluded. The bootstrap destroy path refuses to
delete retained claims or evidence unless
`ALLOW_VALIDATION_EVIDENCE_DELETE=1` is explicitly supplied.
