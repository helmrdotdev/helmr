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
