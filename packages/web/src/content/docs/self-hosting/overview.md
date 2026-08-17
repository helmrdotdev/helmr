---
title: Self-hosting overview
description: Understand the AWS self-hosting architecture, operator responsibilities, and deployment path.
sidebarLabel: Overview
---

# Self-hosting overview

Self-hosted Helmr runs in your AWS account. The public repository supplies reusable Terraform/OpenTofu modules, two example compositions, and release-artifact resolution. You operate the environment around them.

The runtime has three main parts:

| Component | Responsibility |
| --- | --- |
| Control Plane | Serves the web UI and API, authenticates users, coordinates workers, stores run state in PostgreSQL, and writes historical telemetry to ClickHouse. |
| Dispatcher | Reconciles runnable and scheduled work from PostgreSQL authority. |
| Workers | Execute verified Deployment bundles in Firecracker guests. |

The AWS examples compose RDS PostgreSQL, ElastiCache Valkey/Redis, S3, KMS, Secrets Manager, ECS Fargate, an HTTPS load balancer, and optional EC2 Auto Scaling worker groups. A separate bootstrap foundation supplies the immutable Platform Artifact store. ClickHouse is an external, operator-provisioned dependency.

## Choose a deployment path

Use [AWS evaluation](/docs/self-hosting/aws-evaluation/) for a disposable evaluation or proof of concept. Its defaults deliberately trade resilience and retention for lower cost.

Use [AWS production](/docs/self-hosting/aws-production/) as the starting baseline for a customer environment. It strengthens the defaults, but it is not a complete production operating model: remote state, ClickHouse provisioning and networking, credentials, capacity policy, monitoring, backup testing, drift management, and multi-region design remain yours.

Do not promote an evaluation stack in place. Build a production environment from the production baseline and migrate deliberately.

## Deployment sequence

1. Satisfy the [requirements](/docs/self-hosting/requirements/), including bootstrap outputs and external ClickHouse.
2. Configure and apply either the evaluation or production AWS composition with `create_controlplane_service = false`.
3. Check out the exact Product release tag, download its `platform-release.tar`, provenance, and Sigstore bundle, then publish the signed Runtime before enabling Control:

   ```sh
   gh release download "$HELMR_RELEASE_TAG" --pattern 'platform-release*' --dir dist/platform-release
   scripts/publish-platform-release.sh \
     "$PLATFORM_STORE_URI" \
     "$HELMR_RELEASE_TAG" \
     dist/platform-release/platform-release.tar \
     dist/platform-release/platform-release-provenance.json \
     dist/platform-release/platform-release.sigstore.json
   ```

   The publisher verifies the exact tag workflow identity, archive digest and size, checked-out source commit, canonical manifest, and every Runtime object before immutable publication.
4. Configure [authentication](/docs/self-hosting/authentication/) and populate [secrets and data services](/docs/self-hosting/secrets-and-data/).
5. Run the database bootstrap task, then migrations, before enabling services.
6. Start and verify the [Control Plane](/docs/self-hosting/control-plane/).
7. Add [workers](/docs/self-hosting/workers/) when you need task execution.
8. Adopt the checked-in [upgrade procedure](/docs/self-hosting/upgrades/) before changing releases.

The Control Plane can be brought up without workers. Workers are required only
when verified Deployments execute.
