# Helmr AWS Infrastructure

This directory contains provider-generic AWS building blocks and public release
artifact tooling for Helmr. It does not contain the Managed Cloud root stack,
capacity policy, provider credentials, or Cloud validation campaign.

## Layout

- `modules/bootstrap` creates durable release/state foundations.
- `modules/network` creates a reusable VPC and subnet topology.
- `modules/controlplane` creates the Product Control Plane data plane and accepts external
  deployment-owned secret ARNs.
- `modules/release-artifacts` resolves public Control Plane images and Worker AMIs.
- `modules/worker` creates a generic Firecracker Worker host group.
- `modules/worker-image` defines the public Worker AMI build.
- `quickstart` and `standard` are self-hosted compositions.
- `stacks/worker-image` is the public Worker AMI build pipeline.

Self-hosting operators own their surrounding network, ClickHouse service,
capacity policy, credentials, and drift management. Managed Cloud composes
these modules from its private deployment repository without changing their
Product contract.

## Release artifacts

Run Product artifact operations through `scripts/aws-release-artifacts.sh`.
The release workflow publishes a digest-pinned Control Plane image, regional Worker
AMIs, and the signed Platform release. The Control Plane image contains only
`helmr-controlplane` and `helmr-dispatcher`; deployment capacity automation is not a
Product release artifact.

Before enabling or updating Control Plane services, run the database migration task
for the exact image. Keep `/healthz` for process health and use `/readyz` for
traffic readiness after the schema is current.
