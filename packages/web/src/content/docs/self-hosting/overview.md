---
title: Self-hosting overview
description: Understand the AWS self-hosting architecture, operator responsibilities, and deployment path.
sidebarLabel: Overview
---

# Self-hosting overview

Self-hosted Helmr runs in your AWS account. The public repository supplies reusable Terraform/OpenTofu modules, three example compositions, and release-artifact resolution. You operate the environment around them.

The runtime has three main parts:

| Component | Responsibility |
| --- | --- |
| Control Plane | Serves the web UI and API, authenticates users, coordinates workers, stores Session and Turn state in PostgreSQL, and writes historical telemetry to ClickHouse. |
| Dispatcher | Reconciles runnable and scheduled work from PostgreSQL authority. |
| Workers | Execute verified Deployment bundles in Firecracker guests. |

## Choose a deployment path

| Composition | Use |
| --- | --- |
| [Quickstart](/docs/self-hosting/evaluation) | A compact installation: one host for control services, PostgreSQL and ClickHouse, plus separate Workers. Suitable for trying Helmr or continuing to use it without backups. |
| [Standard](/docs/self-hosting/production) | Separate Fargate services, RDS with backup defaults, HTTPS load balancing and operator-owned ClickHouse. |
| [Private](/docs/self-hosting/private) | Private application ingress and restricted platform egress for an existing company network. |

All three consume released Product artifacts and a separately prepared Platform
Artifact store and matching Worker AMI. The operator owns the deployment and its
credentials. Quickstart provides direct host HTTPS; Standard and Private compose
managed AWS services. Network and data protection requirements determine which
composition fits your installation.

Follow the Quickstart page for its host-based startup. The sequence below applies
to Standard and Private.

## Deployment sequence

1. Satisfy the [requirements](/docs/self-hosting/requirements), including bootstrap outputs and external ClickHouse.
2. Configure and apply the Standard or Private AWS composition with `create_controlplane_service = false`.
3. Check out the exact Product release tag, download its signed v0 index, `platform-release.tar`, and provenance, then publish the signed Runtime before enabling Control:

   ```sh
   gh release download "$HELMR_RELEASE_TAG" --pattern 'platform-release*' --pattern 'release-index*' --dir dist/platform-release
   scripts/publish-platform-release.sh \
     "$PLATFORM_STORE_URI" \
     "$HELMR_RELEASE_TAG" \
     dist/platform-release/release-index.json \
     dist/platform-release/release-index.sigstore.json \
     dist/platform-release/platform-release.tar \
     dist/platform-release/platform-release-provenance.json
   ```

   The publisher verifies the signed index workflow identity (exact tag for stable, reviewed main for previews), indexed archive/provenance bytes, archive digest and size, checked-out source commit, canonical manifest, and every Runtime object before immutable publication.
4. Configure [authentication](/docs/self-hosting/authentication) and populate [secrets and data services](/docs/self-hosting/platform-secrets).
5. Run the database bootstrap task, then migrations, before enabling services.
6. Start and verify the [Control Plane](/docs/self-hosting/control-plane).
7. Add [workers](/docs/self-hosting/workers) when you are ready to run agents.
8. Adopt the checked-in [upgrade procedure](/docs/self-hosting/upgrades) before changing releases.

The Control Plane can be brought up without workers. Workers are required only
when verified Deployments execute.
