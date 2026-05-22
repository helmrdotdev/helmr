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

Required: `HELMR_DATABASE_URL`, `HELMR_REDIS_URL`, `HELMR_CAS_URI`, `HELMR_WORKER_TOKEN_SIGNING_KEY`, `HELMR_WORKER_BOOTSTRAP_TOKEN`, `HELMR_AUTH_SECRET`, `HELMR_SECRET_ENCRYPTION_KEY`, `HELMR_GITHUB_APP_ID`, `HELMR_GITHUB_APP_SLUG`, `HELMR_GITHUB_APP_WEBHOOK_SECRET`, `HELMR_GITHUB_APP_CLIENT_ID`, `HELMR_GITHUB_APP_CLIENT_SECRET`, and either `HELMR_GITHUB_APP_PRIVATE_KEY_PATH` or `HELMR_GITHUB_APP_PRIVATE_KEY`.

Deployment mode: `HELMR_DEPLOYMENT_MODE` defaults to `self-hosted`. In `self-hosted` mode, `HELMR_SETUP_TOKEN` is required to create the first and only organization. In `managed-cloud` mode, authenticated users can create organizations without a setup token.

Optional: `HELMR_CONTROL_ADDR`, `HELMR_PUBLIC_URL`, and `HELMR_MAGIC_LINK_DEBUG_URLS`.

Email delivery is disabled by default. Set `HELMR_EMAIL_PROVIDER` to choose a sender:

| Provider | Required variables | Optional variables |
| --- | --- | --- |
| `none` | None. This is the default when no email settings are present. | None |
| `log` | `HELMR_EMAIL_PROVIDER=log` | `HELMR_MAGIC_LINK_DEBUG_URLS=true` logs magic link URLs for local debugging. |
| `smtp` | `HELMR_EMAIL_PROVIDER=smtp`, `HELMR_SMTP_ADDR`, `HELMR_EMAIL_FROM` | `HELMR_SMTP_USERNAME`, `HELMR_SMTP_PASSWORD` |
| `resend` | `HELMR_EMAIL_PROVIDER=resend`, `HELMR_RESEND_API_KEY`, `HELMR_EMAIL_FROM` | None |

`HELMR_EMAIL_FROM` must be an email address or display-name address accepted by the selected provider, such as `Helmr <noreply@example.com>`.

## Dispatcher

Required: `HELMR_DATABASE_URL`, `HELMR_REDIS_URL`.

The AWS control module provisions cluster-mode disabled ElastiCache Valkey/Redis and injects
`HELMR_REDIS_URL` into both `helmr-control` and `helmr-dispatcher`.

## Worker

Required: `HELMR_CONTROL_URL`, `HELMR_CAS_URI`, `HELMR_CHECKPOINT_ENCRYPTION_KEY`, `HELMR_WORKER_FIRECRACKER_JAILER_UID`, and `HELMR_WORKER_FIRECRACKER_JAILER_GID`.

Credential inputs: `HELMR_WORKER_BOOTSTRAP_TOKEN`, `HELMR_WORKER_BOOTSTRAP_TOKEN_PATH`, and `HELMR_WORKER_INSTANCE_CREDENTIAL_PATH`. A worker registers once with a bootstrap token, stores its issued credential in the credential file, and uses that file for later starts. `HELMR_WORKER_RESOURCE_ID` optionally supplies a stable infrastructure resource identity; when omitted, the worker uses the host name.

Runtime inputs include `HELMR_WORKER_WORK_DIR`, `HELMR_WORKER_IMAGES_DIR`, `HELMR_GIT_PATH`, `HELMR_WORKER_BUILDKIT_ADDR`, `HELMR_WORKER_BUILDKIT_CACHE_NAMESPACE`, Firecracker paths and jailer settings, CNI paths/profile, blocked CIDR lists, `HELMR_WORKER_REGION`, `HELMR_WORKER_LABELS`, `HELMR_VM_VCPUS`, `HELMR_VM_MEMORY_MIB`, `HELMR_WORKER_DISK_MIB`, and `HELMR_VM_HEALTH_TIMEOUT`. `HELMR_WORKER_LABELS` is a comma-separated `key=value` list used for placement matching. `HELMR_WORKER_DISK_MIB` overrides the filesystem capacity advertised by filesystem-first worker instances.
