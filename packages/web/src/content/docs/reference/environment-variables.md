---
title: Environment variables
description: Environment variables read by Helmr control, worker, CLI, deploy adapter, and SDK client.
section: Reference
sidebarLabel: Environment variables
order: 960
---

# Environment variables

## CLI and SDK

| Variable | Purpose |
| --- | --- |
| `HELMR_URL` | Control-plane base URL. |
| `HELMR_API_KEY` | Bearer token for CLI or `HelmrClient`. |
| `HELMR_BUN_PATH` | Bun executable used by `helmr deploy`. |
| `HELMR_ADAPTER_PATH` | Deploy adapter entrypoint override. |
| `HELMR_ADAPTER_SDK_PATH` | SDK module path used by the deploy adapter. |

## Control plane

Required: `HELMR_DATABASE_URL`, `HELMR_REDIS_URL`, `HELMR_CAS_URI`, `HELMR_WORKER_TOKEN_SIGNING_KEY`, `HELMR_WORKER_REGISTRATION_TOKEN`, `HELMR_AUTH_SECRET`, `HELMR_SECRET_ENCRYPTION_KEY`, `HELMR_GITHUB_APP_ID`, `HELMR_GITHUB_APP_SLUG`, `HELMR_GITHUB_APP_WEBHOOK_SECRET`, `HELMR_GITHUB_APP_CLIENT_ID`, `HELMR_GITHUB_APP_CLIENT_SECRET`, and either `HELMR_GITHUB_APP_PRIVATE_KEY_PATH` or `HELMR_GITHUB_APP_PRIVATE_KEY`.

Optional: `HELMR_CONTROL_ADDR`, `HELMR_PUBLIC_URL`, `HELMR_SETUP_ENABLED`, `HELMR_BOOTSTRAP_OWNER_EMAIL`, `HELMR_MAGIC_LINK_DEBUG_URLS`, `HELMR_SMTP_ADDR`, `HELMR_SMTP_USERNAME`, `HELMR_SMTP_PASSWORD`, and `HELMR_EMAIL_FROM`.

## Dispatcher

Required: `HELMR_DATABASE_URL`, `HELMR_REDIS_URL`.

The AWS control module provisions cluster-mode disabled ElastiCache Valkey/Redis and injects
`HELMR_REDIS_URL` into both `helmr-control` and `helmr-dispatcher`.

## Worker

Required: `HELMR_CONTROL_URL`, `HELMR_CAS_URI`, `HELMR_CHECKPOINT_ENCRYPTION_KEY`, `HELMR_WORKER_FIRECRACKER_JAILER_UID`, and `HELMR_WORKER_FIRECRACKER_JAILER_GID`.

Credential inputs: `HELMR_WORKER_REGISTRATION_TOKEN`, `HELMR_WORKER_REGISTRATION_TOKEN_PATH`, `HELMR_WORKER_SECRET`, `HELMR_WORKER_CREDENTIAL_PATH`, and `HELMR_WORKER_HOST_ID`.

Runtime inputs include `HELMR_WORKER_WORK_DIR`, `HELMR_WORKER_IMAGES_DIR`, `HELMR_GIT_PATH`, `HELMR_WORKER_BUILDKIT_ADDR`, `HELMR_WORKER_BUILDKIT_CACHE_NAMESPACE`, Firecracker paths and jailer settings, CNI paths/profile, blocked CIDR lists, `HELMR_WORKER_REGION`, `HELMR_WORKER_LABELS`, `HELMR_VM_VCPUS`, `HELMR_VM_MEMORY_MIB`, `HELMR_WORKER_DISK_MIB`, and `HELMR_VM_HEALTH_TIMEOUT`. `HELMR_WORKER_LABELS` is a comma-separated `key=value` list used for placement matching. `HELMR_WORKER_DISK_MIB` overrides the filesystem capacity advertised by filesystem-first worker hosts.
