---
title: Environment variables
description: Environment variables read by Helmr control, worker, CLI, and SDK client.
section: Reference
sidebarLabel: Environment variables
order: 960
---

# Environment variables

## CLI and SDK

| Variable | Purpose |
| --- | --- |
| `HELMR_API_URL` | Control-plane base URL. |
| `HELMR_API_KEY` | Bearer token for CLI or `HelmrClient`. |

## Control plane

Required: `HELMR_DATABASE_URL`, `HELMR_REDIS_URL`, `HELMR_CAS_URI`, `HELMR_CLICKHOUSE_URL`, `WORKER_TOKEN_SIGNING_KEY`, `HELMR_WORKER_GROUPS`, `HELMR_WORKER_GROUP_ID`, `HELMR_REGION_ID`, `HELMR_DEFAULT_REGION_ID`, `HELMR_PROVIDER`, `HELMR_PROVIDER_REGION`, `AUTH_KEY`, `ENCRYPTION_KEY`, `WORKSPACE_FENCING_KEY`, `TOKEN_CREDENTIAL_KEY`, `HELMR_GITHUB_OAUTH_CLIENT_ID`, and `HELMR_GITHUB_OAUTH_CLIENT_SECRET`.

Deployment mode: `HELMR_DEPLOYMENT_MODE` defaults to `self-hosted`. In `self-hosted` mode, `HELMR_SETUP_TOKEN` is required to create the first and only organization. In `managed-cloud` mode, authenticated users can create organizations without a setup token.

`HELMR_WORKER_GROUPS` is the authoritative JSON list of AWS worker-group enrollment policies. Each group identifies its AWS account, region, Auto Scaling group, instance profile, allowed AMIs, and run/build role. The same group and enrollment model is used in both deployment modes.

`HELMR_REGION_ID` and `HELMR_DEFAULT_REGION_ID` are opaque Helmr identifiers, not provider-region or DNS names. Their normalized UTF-8 values must be 1–255 bytes and contain no surrounding whitespace or control characters.

Optional: `HELMR_CONTROL_ADDR`, `HELMR_PUBLIC_URL`, and `HELMR_MAGIC_LINK_DEBUG_URLS`.

ClickHouse telemetry: `HELMR_CLICKHOUSE_URL` is required. Set `HELMR_CLICKHOUSE_USER` when the service user is not `default`, and set `HELMR_CLICKHOUSE_PASSWORD` when the service requires a password.

`AUTH_KEY`, `TOKEN_CREDENTIAL_KEY`, `WORKSPACE_FENCING_KEY`,
`ENCRYPTION_KEY`, and `WORKER_TOKEN_SIGNING_KEY` are distinct single roots.
Each must be base64 and decode to exactly 32 bytes. Every Control replica uses
the same values. Online rotation and multi-key verification are not supported.

Email delivery is disabled by default. Set `HELMR_EMAIL_PROVIDER` to choose a sender:

| Provider | Required variables | Optional variables |
| --- | --- | --- |
| `none` | None. This is the default when no email settings are present. | None |
| `log` | `HELMR_EMAIL_PROVIDER=log` | `HELMR_MAGIC_LINK_DEBUG_URLS=true` logs magic link URLs for local debugging. |
| `smtp` | `HELMR_EMAIL_PROVIDER=smtp`, `HELMR_SMTP_ADDR`, `HELMR_EMAIL_FROM` | `HELMR_SMTP_USERNAME`, `HELMR_SMTP_PASSWORD` |
| `resend` | `HELMR_EMAIL_PROVIDER=resend`, `HELMR_RESEND_API_KEY`, `HELMR_EMAIL_FROM` | None |

`HELMR_EMAIL_FROM` must be an email address or display-name address accepted by the selected provider, such as `Helmr <noreply@example.com>`.

## Dispatcher

Required: `HELMR_DATABASE_URL`, `HELMR_REDIS_URL`, `HELMR_CLICKHOUSE_URL`,
and `WORKSPACE_FENCING_KEY`.

`ENCRYPTION_KEY` is control-plane authority and is not provided to the dispatcher.

The dispatcher uses the same single base64-encoded 32-byte
`WORKSPACE_FENCING_KEY` as the control service.

The AWS control module provisions cluster-mode disabled ElastiCache Valkey/Redis and injects
`HELMR_REDIS_URL` into both `helmr-control` and `helmr-dispatcher`.

Optional Run placement tuning:

| Variable | Default | Purpose |
| --- | --- | --- |
| `HELMR_RUN_PREPARATION_LIMIT` | `32` | Maximum concurrent Run runtime preparations in one queue scope before applying any lower pinned queue limit. |
| `HELMR_RUN_RESERVATION_TTL` | `5m` | Lifetime of a cold runtime reservation before fenced cleanup is required. |
| `HELMR_RUN_LEASE_START_DEADLINE` | `1m` | Time allowed for a worker to claim a newly assigned Run Lease. |
| `HELMR_RUN_LEASE_TTL` | `5m` | Operational lifetime sampled when a Run and Workspace Lease are granted or renewed. Must be at least the start deadline. |

Optional Schedule worker tuning:

| Variable | Default | Purpose |
| --- | --- | --- |
| `HELMR_SCHEDULE_POLL_INTERVAL` | `1s` | How often the dispatcher claims due Schedule cursors from PostgreSQL. |
| `HELMR_SCHEDULE_CLAIM_LIMIT` | `100` | Maximum due Schedule rows claimed per poll; must be an integer from 1 through 2147483647. |
| `HELMR_SCHEDULE_CONCURRENCY` | `10` | Maximum concurrent Schedule admission transactions per dispatcher; must be an integer from 1 through 2147483647. |
| `HELMR_SCHEDULE_CLAIM_LEASE` | `5m` | PostgreSQL claim lease held while one Schedule cursor is admitted. |

## Worker

Required: `HELMR_CONTROL_URL`, `HELMR_CAS_URI`, `HELMR_WORKER_GROUP_ID`, `CHECKPOINT_ENCRYPTION_KEY`, `HELMR_WORKER_FIRECRACKER_JAILER_UID`, and `HELMR_WORKER_FIRECRACKER_JAILER_GID`.

The worker requests a one-time enrollment challenge and proves its AWS EC2 identity with the instance identity document and a nonce-bound signed STS request. Control verifies the instance against the configured worker-group policy, then issues a renewable worker credential stored at `HELMR_WORKER_INSTANCE_CREDENTIAL_PATH`. No deployment-mode or shared bootstrap credential is accepted by the worker.

Runtime inputs include `HELMR_WORKER_WORK_DIR`, `HELMR_WORKER_IMAGES_DIR`, `HELMR_GIT_PATH`, Firecracker paths and jailer settings, routed-network link and translation pools, resolver and blocked CIDRs, `HELMR_VM_VCPUS`, `HELMR_VM_MEMORY_MIB`, `HELMR_WORKER_DISK_MIB`, and `HELMR_VM_HEALTH_TIMEOUT`. `HELMR_WORKER_DISK_MIB` overrides the filesystem capacity advertised by filesystem-first worker instances. Workspace-image builds start the pinned BuildKit daemon inside a fresh image-build guest; there is no host BuildKit address or service setting.
