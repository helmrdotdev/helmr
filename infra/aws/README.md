# Helmr AWS Infrastructure

This directory contains reusable AWS building blocks and public release artifact
tooling for Helmr.

## Layout

- `modules/bootstrap` is the reusable deployment foundation child module.
- `modules/network` creates a reusable VPC and subnet topology.
- `modules/controlplane` creates the Product Control Plane data plane and accepts external
  deployment-owned secret ARNs.
- `modules/release-artifacts` resolves public Control Plane images and Worker AMIs.
- `modules/worker` creates a generic Firecracker Worker host group.
- `modules/worker-image` defines the public Worker AMI build.
- `quickstart` and `standard` are self-hosted compositions.
- `stacks/release-build` is the standalone OSS release artifact foundation.
- `stacks/worker-image` is the public Worker AMI build pipeline.

Self-hosting operators own their surrounding network, ClickHouse service,
capacity policy, credentials, backups, upgrades, recovery, and drift
management. Pin one exact Helmr release cohort; availability of an older
release does not guarantee a safe downgrade of runtime or persisted data.

## Permissions boundaries

Self-hosted compositions accept optional caller-owned customer-managed IAM
permissions boundaries:

- `permissions_boundary_arn` on `quickstart` and `standard` attaches to every
  Control Plane IAM role through the controlplane module.
- `external_permissions_boundary_arn` on those stacks applies only to the current
  Worker generation through the worker module. Retained external identity lives
  in `sealed_provider_definition.boundary_policy_arn` only; changing the current
  stack input updates the current generation in place rather than rotating the Pool.

The caller owns external policy correctness and immutability. Effective role
authority is always the intersection of the boundary with each role's inline or
managed policies. A boundary alone does not guarantee isolation: resource-based
policies, session grants, and protected-role constraints still matter.

External Worker boundaries are read through `aws_iam_policy` during plan/apply.
The deployment role therefore needs `iam:GetPolicy` and `iam:GetPolicyVersion` on
the external policy ARN. Sealed Worker generations record `boundary_policy_arn`
(when external) and verified policy bytes; external drift or caller input changes
away from a sealed external boundary fail before launch.

Image Builder accepts the same optional `permissions_boundary_arn` input
through `stacks/worker-image`.

## Release artifacts

Run release-build foundation and Worker image operations through
`scripts/aws-release-artifacts.sh`.
The release workflow publishes a digest-pinned Control Plane image, regional Worker
AMIs, and the signed Platform release. The Control Plane image contains only
`control-plane` and `dispatcher`; deployment capacity automation is not a
Product release artifact. The Control Plane and bundle-builder GHCR packages
are public and should be consumed by immutable digest.

Before enabling or updating Control Plane services, run the database migration task
for the exact image. Keep `/healthz` for process health and use `/readyz` for
traffic readiness after the schema is current.
