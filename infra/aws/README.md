# Helmr AWS Infrastructure

This directory contains the AWS infrastructure entrypoint for Helmr.

Terraform/OpenTofu manages AWS resources. It does not build release artifacts. Customer-facing
examples resolve the official control image and region-specific worker AMI from a versioned release
manifest, while the dev stacks keep the local build and AMI pipeline workflows.

## Layout

- `modules/bootstrap` creates a state bucket for teams that do not already have a backend.
- `modules/network` creates the VPC, public subnets, private subnets, and NAT gateway.
- `modules/control` creates control-plane AWS dependencies such as Postgres, CAS storage, and
  secret placeholders.
- `modules/release-artifacts` resolves the official control image and worker AMI for a Helmr
  release.
- `modules/worker` creates the EC2 worker role, launch template, and Auto Scaling group.
- `modules/worker-image` creates an EC2 Image Builder pipeline for the worker AMI.
- `stacks/dev` is the deployable AWS development and full-run smoke environment.
- `stacks/worker-image` is the deployable worker AMI build pipeline.
- `examples` is reserved for customer-facing variants by enterprise or use case.

For full-run smoke testing, start from `stacks/dev/full-run-smoke.tfvars.example`. It keeps
worker capacity to one host and enables EC2 nested virtualization for supported C8i/M8i/R8i
instances, so Firecracker can be exercised without a large bare-metal worker.

Use `scripts/aws-dev-smoke.sh` from the repository root to reproduce the worker AMI and dev stack
workflow without storing AWS credentials or secret values in the repository.

Typical flow:

```sh
nix develop .#infra
scripts/aws-dev-smoke.sh check
scripts/aws-dev-smoke.sh bootstrap-init
scripts/aws-dev-smoke.sh bootstrap-apply
eval "$(scripts/aws-dev-smoke.sh bootstrap-output)"
scripts/aws-dev-smoke.sh source-bundle
scripts/aws-dev-smoke.sh worker-image-source-check
scripts/aws-dev-smoke.sh worker-image-init
scripts/aws-dev-smoke.sh worker-image-apply
scripts/aws-dev-smoke.sh worker-image-start
scripts/aws-dev-smoke.sh worker-image-wait
scripts/aws-dev-smoke.sh dev-tfvars
# Fill infra/aws/stacks/dev/full-run-smoke.tfvars with non-credential deployment values.
scripts/aws-dev-smoke.sh dev-init
scripts/aws-dev-smoke.sh dev-apply
scripts/aws-dev-smoke.sh dev-secrets
scripts/aws-dev-smoke.sh dev-migrate
```

## Deployment

Run the control migration task for the image before enabling or updating the control service. Keep
the target group health check on `/healthz` while rolling out an older image; use `/readyz` once the
deployed image serves readiness checks so tasks only receive traffic after the database schema has
been migrated to at least the version required by that binary.

## Release Artifacts

AWS examples resolve release inputs from `aws-artifacts.json` attached to the GitHub Release for the
selected `helmr_version`. The release workflow publishes:

- `ghcr.io/helmrdotdev/helmr-control:<version>` and records its immutable digest in
  `aws-artifacts.json`.
- `worker_amis`, a JSON object keyed by AWS region.

Worker AMIs are built through the Image Builder stack because they are AWS account and region
artifacts. After the AMI build completes, rerun the release workflow manually for the same tag with
`worker_amis_json`, for example:

```json
{"us-east-1":"ami-0123456789abcdef0"}
```

Guest boot artifacts are still built and released by `.github/workflows/boot-artifacts.yaml`; the
worker AMI build embeds those artifacts under `/var/lib/helmr/images/guest/out`.
