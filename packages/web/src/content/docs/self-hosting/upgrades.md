---
title: Upgrades
description: Upgrade release-pinned Control Plane and worker artifacts with migrations, draining, and explicit rollback planning.
---

# Upgrades

Treat a Helmr upgrade as a coordinated change to immutable release artifacts, database schema, and worker hosts. The repository resolves an AWS release manifest from the exact `helmr_version`; the manifest supplies a digest-pinned Control Plane image and regional worker AMI IDs.

## Before changing the version

1. Record the current `helmr_version`, `controlplane_image`, `worker_ami_id`, `release_artifacts_manifest_url`, and Terraform/OpenTofu plan.
2. Confirm the target manifest has a Control Plane image pinned with `@sha256:<digest>` and a worker AMI for your region if workers are enabled.
3. Back up state and data according to your operating policy, and verify a restore separately when the change warrants it.
4. Rehearse the migration and worker replacement in a non-production environment.

Custom Control Plane overrides must also be digest-pinned. A custom AMI ID is only a locator: the Control Plane does not attest or allowlist that image, so validating the AMI contract and provenance is your responsibility.

## Upgrade the Control Plane

Set the target `helmr_version` and inspect the plan without enabling a new service image prematurely. Run the database migration task for the exact target image and wait for a zero container exit code before updating the long-running Control Plane and dispatcher services. Then apply the service update and verify both `/healthz` and `/readyz`.

The checked-in flow establishes migration-before-service ordering, but it does not provide an automatic schema rollback. Do not assume that reverting the image also reverts the database. Decide whether the target migration is backward compatible and define a restore-based recovery point before rollout.

## Replace workers

When the target release changes the worker AMI, apply the new launch template but do not assume instances will refresh automatically. Drain each exact logical worker to `termination_ready` before provider deletion, then explicitly coordinate the Auto Scaling instance refresh. Preserve enough old capacity to serve work until replacement workers authenticate and become active.

Checkpoint restore validates runtime compatibility, including runtime and rootfs digests and resource shape. Existing checkpoints may not resume on an incompatible replacement worker; the checked-in flow does not promise cross-release checkpoint conversion.

## Rollback limits

A practical rollback may require all of the following:

- restoring the prior digest-pinned Control Plane image;
- restoring the prior worker AMI and replacing hosts through the same drain path;
- restoring the database if the migration is not backward compatible;
- retaining the same root and checkpoint encryption keys;
- accepting that already-created artifacts or checkpoints may be incompatible with the older release.

Because these steps are not automated as one transaction, define the rollback decision point and data recovery procedure before the upgrade. If `/readyz`, worker activation, or a smoke run fails, stop the rollout and use that prepared procedure rather than improvising an in-place downgrade.
