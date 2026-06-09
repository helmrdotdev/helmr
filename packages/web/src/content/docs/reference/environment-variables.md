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
| `HELMR_ADAPTER_RUNTIME_PATH` | Adapter runtime executable used by `helmr deploy`. |
| `HELMR_ADAPTER_CACHE_DIR` | Directory used to materialize the embedded deploy adapter before invoking the runtime. |
| `HELMR_ADAPTER_PATH` | Development override for the adapter entrypoint. Must be set with `HELMR_ADAPTER_REGISTER_PATH`. |
| `HELMR_ADAPTER_REGISTER_PATH` | Development override for the adapter register hook. Must be set with `HELMR_ADAPTER_PATH`. |

`HELMR_ADAPTER_CACHE_DIR` should point to a user-private directory when overridden.

## Control plane

Required: `HELMR_DATABASE_URL`, `HELMR_REDIS_URL`, `HELMR_CAS_URI`, `HELMR_WORKER_TOKEN_SIGNING_KEY`, `HELMR_WORKER_BOOTSTRAP_TOKEN`, `HELMR_AUTH_SECRET`, `HELMR_SECRET_ENCRYPTION_KEY`, `HELMR_GITHUB_OAUTH_CLIENT_ID`, and `HELMR_GITHUB_OAUTH_CLIENT_SECRET`.

Deployment mode: `HELMR_DEPLOYMENT_MODE` defaults to `self-hosted`. In `self-hosted` mode, `HELMR_SETUP_TOKEN` is required to create the first and only organization. In `managed-cloud` mode, authenticated users can create organizations without a setup token.

Optional: `HELMR_CONTROL_ADDR`, `HELMR_PUBLIC_URL`, and `HELMR_MAGIC_LINK_DEBUG_URLS`.

`HELMR_SECRET_ENCRYPTION_KEY_OLD` is optional and should only be set during
Helmr-managed secret key rotation. While it is set, control and dispatcher can
decrypt secrets written with the old key, and new writes use
`HELMR_SECRET_ENCRYPTION_KEY`. Run `helmr-control secrets reencrypt` to rewrite
old-key secrets before removing `HELMR_SECRET_ENCRYPTION_KEY_OLD`; repeat the
command until `remaining_old_key_count` is `0`.

When using the AWS module with `secret_encryption_key_old_arn`, also set
`secret_encryption_key_old_kms_key_arns` if that old-key secret uses a
customer-managed KMS key other than the module KMS key.

Email delivery is disabled by default. Set `HELMR_EMAIL_PROVIDER` to choose a sender:

| Provider | Required variables | Optional variables |
| --- | --- | --- |
| `none` | None. This is the default when no email settings are present. | None |
| `log` | `HELMR_EMAIL_PROVIDER=log` | `HELMR_MAGIC_LINK_DEBUG_URLS=true` logs magic link URLs for local debugging. |
| `smtp` | `HELMR_EMAIL_PROVIDER=smtp`, `HELMR_SMTP_ADDR`, `HELMR_EMAIL_FROM` | `HELMR_SMTP_USERNAME`, `HELMR_SMTP_PASSWORD` |
| `resend` | `HELMR_EMAIL_PROVIDER=resend`, `HELMR_RESEND_API_KEY`, `HELMR_EMAIL_FROM` | None |

`HELMR_EMAIL_FROM` must be an email address or display-name address accepted by the selected provider, such as `Helmr <noreply@example.com>`.

## Dispatcher

Required: `HELMR_DATABASE_URL`, `HELMR_REDIS_URL`, `HELMR_AUTH_SECRET`, and `HELMR_SECRET_ENCRYPTION_KEY`.

Set `HELMR_SECRET_ENCRYPTION_KEY_OLD` on the dispatcher during the same rotation
window as control so scheduled runs can resolve old-key secrets until
re-encryption completes.

The AWS control module provisions cluster-mode disabled ElastiCache Valkey/Redis and injects
`HELMR_REDIS_URL` into both `helmr-control` and `helmr-dispatcher`.

Optional schedule worker tuning:

| Variable | Default | Purpose |
| --- | --- | --- |
| `HELMR_SCHEDULE_REPAIR_EVERY` | `5s` | How often the dispatcher repairs schedule Redis entries from the database and drains due entries. |
| `HELMR_SCHEDULE_REPAIR_LIMIT` | `100` | Schedule repair page size and due-entry dequeue batch size. |
| `HELMR_SCHEDULE_TRIGGER_CONCURRENCY` | `10` | Maximum concurrent schedule trigger attempts per dispatcher. |
| `HELMR_SCHEDULE_REPAIR_LOOKAHEAD` | `40s` | Safety-net window of upcoming next-fire entries repaired into Redis. Steady-state schedules enqueue their next fire directly. |
| `HELMR_SCHEDULE_LEASE` | `5m` | Redis lease duration for a due schedule fire. |
| `HELMR_SCHEDULE_MAX_ATTEMPTS` | `10` | Retry attempts before the current schedule fire is skipped. |
| `HELMR_SCHEDULE_JITTER` | `30s` | Stable per-schedule jitter applied when registering next-fire entries. |

## Worker

Required: `HELMR_CONTROL_URL`, `HELMR_CAS_URI`, `HELMR_CHECKPOINT_ENCRYPTION_KEY`, `HELMR_WORKER_FIRECRACKER_JAILER_UID`, and `HELMR_WORKER_FIRECRACKER_JAILER_GID`.

Credential inputs: `HELMR_WORKER_BOOTSTRAP_TOKEN`, `HELMR_WORKER_BOOTSTRAP_TOKEN_PATH`, and `HELMR_WORKER_INSTANCE_CREDENTIAL_PATH`. A worker registers once with a bootstrap token, joins the token's worker group, stores its issued credential in the credential file, and uses that file for later starts. `HELMR_WORKER_RESOURCE_ID` optionally supplies a stable infrastructure resource identity; when omitted, the worker uses the host name.

Runtime inputs include `HELMR_WORKER_WORK_DIR`, `HELMR_WORKER_IMAGES_DIR`, `HELMR_GIT_PATH`, `HELMR_WORKER_BUILDKIT_ADDR`, `HELMR_WORKER_BUILDKIT_CACHE_NAMESPACE`, Firecracker paths and jailer settings, CNI paths/profile, blocked CIDR lists, `HELMR_WORKER_REGION`, `HELMR_WORKER_LABELS`, `HELMR_VM_VCPUS`, `HELMR_VM_MEMORY_MIB`, `HELMR_WORKER_DISK_MIB`, and `HELMR_VM_HEALTH_TIMEOUT`. `HELMR_WORKER_LABELS` is a comma-separated `key=value` list used for placement matching. `HELMR_WORKER_DISK_MIB` overrides the filesystem capacity advertised by filesystem-first worker instances.
