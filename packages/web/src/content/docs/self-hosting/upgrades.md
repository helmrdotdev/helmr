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

The steps above apply when the target release keeps the worker API contract revision. Old and new workers then both serve the upgraded Control Plane during the replacement.

## Worker API contract changes

Every worker request names the worker API contract it was built for, such as `helmr.worker-api.v1.r1`, and the Control Plane rejects any other or missing contract with `worker_contract_mismatch`. When the target release changes that revision, old workers cannot serve the upgraded Control Plane, so the Control Plane-first order above does not apply. The first release that checks the contract changes it for every existing worker; later releases change it only for incompatible worker API changes. The non-production rehearsal shows whether a target release changes it: after the Control Plane upgrade, old workers fail with `worker_contract_mismatch`.

For such a release, cut over in this order:

1. Before upgrading the Control Plane, drain the old workers to `termination_ready` against the old Control Plane, so their Runs and Computers are handed over or finished under the contract they speak.
2. Run the migration and upgrade the Control Plane as above.
3. Launch replacement workers from the target release's AMI and wait for them to authenticate and become active.

Old workers cannot keep serving while the new Control Plane runs, so plan the capacity gap explicitly: either accept an interruption between the drain and the replacements' activation, or stage separate capacity, such as a second worker group for the target release, that is ready as soon as the Control Plane changes. Workers that are still running when the Control Plane changes are rejected on their next request and fenced about two minutes later.

Checkpoint restore validates runtime compatibility, including runtime and rootfs digests and resource shape. Existing checkpoints may not resume on an incompatible replacement worker; the checked-in flow does not promise cross-release checkpoint conversion.

## Rollback limits

A practical rollback may require all of the following:

- restoring the prior digest-pinned Control Plane image;
- restoring the prior worker AMI and replacing hosts through the same drain path;
- restoring the database if the migration is not backward compatible;
- retaining the same root and checkpoint encryption keys;
- accepting that already-created artifacts or checkpoints may be incompatible with the older release.

Because these steps are not automated as one transaction, define the rollback decision point and data recovery procedure before the upgrade. If `/readyz`, worker activation, or a smoke run fails, stop the rollout and use that prepared procedure rather than improvising an in-place downgrade.
