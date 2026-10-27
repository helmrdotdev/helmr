# Helmr AWS Infrastructure

This directory contains reusable AWS building blocks and public release artifact
tooling for Helmr.

## Layout

- `modules/release-storage` owns reusable release buckets, encryption keys and the Control Plane image repository.
- `modules/release-publisher` owns a bounded publisher role for explicitly selected storage and trusted IAM principals.
- `modules/network` creates a reusable VPC and subnet topology.
- `modules/controlplane` creates the Product Control Plane data plane and accepts external
  deployment-owned secret ARNs.
- `modules/release-artifacts` resolves public Control Plane images and Worker AMIs.
- `modules/worker` creates a generic Firecracker Worker host group.
- `modules/worker-image` defines the public Worker AMI build.
- `quickstart` and `standard` are public self-hosted compositions.
- [`private`](private/README.md) is a private-ingress deployment reference with restricted platform egress.
- `stacks/release-build` is the standalone OSS release artifact foundation.
- `stacks/worker-image` is the public Worker AMI build pipeline.

Self-hosting operators own their surrounding network, ClickHouse service,
capacity policy, credentials, backups, upgrades, recovery, and drift
management. Pin one exact Helmr release cohort; availability of an older
release does not guarantee a safe downgrade of runtime or persisted data.

## Release artifacts

The release workflow publishes a digest-pinned Control Plane image, signed
Platform release, and host/runtime bundles. Common releases do not publish
regional Worker AMIs. Operators can use the public
[Worker image stack](stacks/worker-image/README.md) to build a private AMI from
those verified bundles. `scripts/aws-release-artifacts.sh` owns release-publisher foundation operations
and Product-owned image builds from the current checkout. Its image commands
are not the operator path for consuming already published bundles. The Control Plane image contains only
`control-plane` and `dispatcher`; deployment capacity automation is not a
Product release artifact. The Control Plane and bundle-builder GHCR packages
are public and should be consumed by immutable digest.

Before enabling or updating Control Plane services, run the database migration task
for the exact image. Keep `/healthz` for process health and use `/readyz` for
traffic readiness after the schema is current.

## Optional Slack integration

The deployment operator sets `slack_transport` (`http`, `socket` or `disabled`).
Quickstart and standard default to HTTP; the private reference uses Socket Mode.
The same Product image supports all modes. Organization owners/admins create a
dedicated app per Agent/Environment and save credentials through Console; app
secrets and installation tokens are encrypted in PostgreSQL, not Terraform.

`APP_ORIGIN` is the canonical browser/API origin. HTTP callbacks use
`/integrations/slack/apps/{registrationID}/events` and `/interactions` and need
Slack-reachable HTTPS. Socket Mode opens an outbound WSS connection and mounts
neither receiver. Browser installation and identity linking still return to
`/auth/slack/callback` and `/api/slack/user-links/callback`; users must be on the
company network for a private origin. See the Product [Slack setup guide](../../packages/web/src/content/docs/guides/connect-slack.md).

The optional `outbound_proxy_url` input is an explicit HTTP CONNECT path for
GitHub, Slack and Resend clients. Private service/AWS clients use direct paths.
A proxy failure never falls back to direct internet access. Restricting the
network remains the operator's responsibility; see the private reference for
service endpoints, DNS policy and startup dependencies.

## IAM ceilings and deployment recovery

The Control Plane, Worker and Worker-image modules accept one optional
`permissions_boundary_arn` for a customer-managed policy in the caller's AWS
account and partition. The caller owns that policy as an account ceiling; each
module still owns its least-privilege role grants. Supplying a boundary does not
replace or expand those grants. Control Plane attaches it to all six roles;
Worker-image attaches it to the image-builder role, including through the
standalone image stack. Invalid supplied authority never selects a default.
Worker retains its generated ceiling when the input is null; see its
[module contract](modules/worker/README.md).

Control Plane and dispatcher ECS circuit breakers remain enabled. The generic
`enable_deployment_rollback` input defaults to `true` and is forwarded by quickstart
and standard. This allows recovery to the preceding task definition when its code
is compatible with the current data. Automatic rollback does not restore a database
or establish schema compatibility. Operators must apply `false` before incompatible
migrations or resets, then use roll-forward recovery. Run migrations from the exact
selected image before enabling or updating services.

## Operator-owned release foundation

Compose storage and publication under your own root/backend. The quickstart and
standard roots accept the resulting Platform store inputs; they do not create this
foundation. For example, with this checkout at `./helmr` relative to your root:

```hcl
module "storage" {
  source = "./helmr/infra/aws/modules/release-storage"
  name   = "my-helmr"
}

module "publisher" {
  source                              = "./helmr/infra/aws/modules/release-publisher"
  name                                = "my-helmr"
  principal_arns                      = [var.publisher_principal_arn]
  platform_store_bucket_arn           = module.storage.platform_store_bucket_arn
  platform_store_kms_key_arn          = module.storage.platform_store_kms_key_arn
  controlplane_release_repository_arn = module.storage.controlplane_release_repository_arn
}
```

The operator supplies an existing trusted IAM role/user ARN. Publication grants
remain limited to the selected Platform objects/key and Control Plane repository;
storage immutability is enforced by storage policy independently of that role.
An operator with separately owned delivery IAM can consume storage alone.

The network module's optional `retain_nat_eip` keeps its existing allocation when
`enable_nat_gateway=false`. The gateway and private default route disappear; S3
Gateway endpoints and VPC resources remain. Re-enabling NAT uses the retained
allocation. The default is false; set both flags false for final allocation
retirement. This switch does not stop services, drain work or make a private
workload usable without outbound connectivity. Coordinate workload closure and
restoration before changing NAT lifetime.

## Diagnostic process limits

All three self-host roots forward `controlplane_environment` and
`dispatcher_environment` as non-secret maps to the actual ECS containers.
Both processes require explicit diagnostic admission limits:
`DIAGNOSTIC_CHUNK_BYTES`, `DIAGNOSTIC_SOURCE_BYTES`, `DIAGNOSTIC_SOURCE_RECORDS`,
`DIAGNOSTIC_ENVIRONMENT_BYTES`, `DIAGNOSTIC_ENVIRONMENT_RECORDS`,
`DIAGNOSTIC_QUEUE_BYTES`, `DIAGNOSTIC_QUEUE_RECORDS`, and
`DIAGNOSTIC_DB_MAX_CONNECTIONS`. Dispatcher additionally requires
`DIAGNOSTIC_EXPORT_BATCH_BYTES` and `DIAGNOSTIC_EXPORT_BATCH_RECORDS`.

Each root's `terraform.tfvars.example` lists explicit values from
[`dev/runtime/config.example.json`](../../dev/runtime/config.example.json).
These are development examples, not production defaults. Choose values for the
actual workload, retained queue/storage budget and database connection budget;
keep chunk/source/environment/queue bounds ordered, and ensure export batches
fit at least one chunk. The roots infer no diagnostic values and provide no
fallback. Missing or non-positive limits fail planning before a nonzero service starts. Bootstrap-only and zero-count services can remain inert without diagnostic limits. The process validates relationships between configured bounds at startup.
Do not place secrets in either environment map; use the module's explicit secret
inputs for managed credentials.

Worker startup also requires explicit `WORKER_LOG_CHUNK_BYTES`,
`WORKER_LOG_BUFFER_BYTES` and `WORKER_LOG_BUFFER_RECORDS` in the root's
`worker_environment` map. Each root example uses the development fixture's
65536-byte chunks, 1048576-byte buffer and 256-record buffer; choose actual values
for the Worker budget. This map belongs to immutable Pool generation inputs.
Changing it creates a new Pool identity. Retained generations keep their own map
and exact sealed user-data; the current map is never applied globally to them.

New Worker launch definitions require explicit positive log limits even when the
ASG starts at zero capacity; the launch definition must be ready for activation.
`create_worker = false` roots remain inert without instantiating that module.
Retained sealed user-data remains authoritative. Shared modules also reject guest
network CIDRs that are not ordered by numeric IPv4 address and prefix length,
matching the runtime configuration contract.
