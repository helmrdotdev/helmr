---
title: Environment variables
description: Environment variables read by Helmr control, worker, CLI, project config inspector, and SDK client.
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
| `HELMR_CONFIG_RUNTIME_PATH` | Node executable used to inspect `helmr.config.ts`. |
| `HELMR_CONFIG_CACHE_DIR` | Directory used to materialize the embedded config inspector. |
| `HELMR_CONFIG_INSPECTOR_PATH` | Development override for the config inspector. Must be set with `HELMR_CONFIG_REGISTER_PATH`. |
| `HELMR_CONFIG_REGISTER_PATH` | Development override for the TypeScript register hook. Must be set with `HELMR_CONFIG_INSPECTOR_PATH`. |

`HELMR_CONFIG_CACHE_DIR` should point to a user-private directory when overridden.

## Control plane

Required: `HELMR_DATABASE_URL`, `HELMR_REDIS_URL`, `HELMR_CAS_URI`, `HELMR_CLICKHOUSE_URL`, `HELMR_WORKER_TOKEN_SIGNING_KEY`, `HELMR_WORKER_GROUPS`, `HELMR_WORKER_GROUP_ID`, `HELMR_REGION_ID`, `HELMR_DEFAULT_REGION_ID`, `HELMR_AUTH_SECRET`, `HELMR_SECRET_ENCRYPTION_KEY`, `HELMR_LOOKUP_HMAC_KEYS`, `HELMR_WORKSPACE_FENCING_KEY_FINGERPRINT`, `HELMR_WORKSPACE_FENCING_KEYS`, `HELMR_TOKEN_CREDENTIAL_KEY_ID`, `HELMR_TOKEN_CREDENTIAL_KEYS`, `HELMR_GITHUB_OAUTH_CLIENT_ID`, and `HELMR_GITHUB_OAUTH_CLIENT_SECRET`.

Deployment mode: `HELMR_DEPLOYMENT_MODE` defaults to `self-hosted`. In `self-hosted` mode, `HELMR_SETUP_TOKEN` is required to create the first and only organization. In `managed-cloud` mode, authenticated users can create organizations without a setup token.

`HELMR_WORKER_GROUPS` is the authoritative JSON list of AWS worker-group enrollment policies. Each group identifies its AWS account, region, Auto Scaling group, instance profile, allowed AMIs, and run/build role. The same group and enrollment model is used in both deployment modes.

`HELMR_REGION_ID` and `HELMR_DEFAULT_REGION_ID` are opaque Helmr identifiers, not provider-region or DNS names. Their normalized UTF-8 values must be 1–255 bytes and contain no surrounding whitespace or control characters.

Optional: `HELMR_CONTROL_ADDR`, `HELMR_PUBLIC_URL`, and `HELMR_MAGIC_LINK_DEBUG_URLS`.

ClickHouse telemetry: `HELMR_CLICKHOUSE_URL` is required. Set `HELMR_CLICKHOUSE_USER` when the service user is not `default`, and set `HELMR_CLICKHOUSE_PASSWORD` when the service requires a password.

`HELMR_SECRET_ENCRYPTION_KEY_OLD` is optional and should only be set during
Helmr-managed secret key rotation. While it is set, control can decrypt secrets
written with the old key, and new writes use
`HELMR_SECRET_ENCRYPTION_KEY`. Run `helmr-control secrets reencrypt` to rewrite
old-key secrets before removing `HELMR_SECRET_ENCRYPTION_KEY_OLD`; repeat the
command until `remaining_old_key_count` is `0`.

`HELMR_LOOKUP_HMAC_KEYS` is a JSON object from positive integer versions to
base64-encoded 32-byte keys, for example `{"1":"<base64-key>"}`. It contains
key bytes only; PostgreSQL selects the active versions and the current write
version. After adding a new key to every Control instance, run
`helmr-control lookup-hmac activate --version <new-version>`. Then run
`helmr-control secrets reauthenticate --from-version <old-version>` until
`remaining_old_key_count` is `0`. Then run
`helmr-control lookup-hmac collect-claims` until both reported counts are `0`,
followed by
`helmr-control lookup-hmac key-usage`. When both the old version's
`claim_count` and `secret_version_count` are `0`, run
`helmr-control lookup-hmac retire --version <old-version>` before removing its
bytes from configuration or KMS. A new database must receive its first explicit
`lookup-hmac activate` before Control starts.

`HELMR_TOKEN_CREDENTIAL_KEYS` is a JSON object from content-derived
`sha256:<hex>` key IDs to base64-encoded 32-byte keys.
`HELMR_TOKEN_CREDENTIAL_KEY_ID` selects the active derivation key. Keep every
key referenced by a Token or its public completion credential readable; Control
readiness fails closed when a referenced key is absent.

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

Required: `HELMR_DATABASE_URL`, `HELMR_REDIS_URL`, `HELMR_CLICKHOUSE_URL`,
`HELMR_WORKSPACE_FENCING_KEY_FINGERPRINT`, and
`HELMR_WORKSPACE_FENCING_KEYS`.

`HELMR_LOOKUP_HMAC_KEYS` and Secret encryption keys are control-plane
authority and are not provided to the dispatcher.

The dispatcher uses the same content-addressed Workspace fencing key ring as
the control service. The fingerprint selects the active write key.
`HELMR_WORKSPACE_FENCING_KEYS` is a JSON object from key fingerprint to
base64-encoded 32-byte key. Every key referenced by a nonterminal Workspace
Lease must remain readable during rollout and retirement.

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

Required: `HELMR_CONTROL_URL`, `HELMR_CAS_URI`, `HELMR_WORKER_GROUP_ID`, `HELMR_WORKER_PROVIDER_REGION`, `HELMR_CHECKPOINT_ENCRYPTION_KEY`, `HELMR_WORKER_FIRECRACKER_JAILER_UID`, and `HELMR_WORKER_FIRECRACKER_JAILER_GID`.

The worker requests a one-time enrollment challenge and proves its AWS EC2 identity with the instance identity document and a nonce-bound signed STS request. Control verifies the instance against the configured worker-group policy, then issues a renewable worker credential stored at `HELMR_WORKER_INSTANCE_CREDENTIAL_PATH`. No deployment-mode or shared bootstrap credential is accepted by the worker.

Runtime inputs include `HELMR_WORKER_WORK_DIR`, `HELMR_WORKER_IMAGES_DIR`, `HELMR_GIT_PATH`, `HELMR_WORKER_BUILDKIT_ADDR`, `HELMR_WORKER_BUILDKIT_CACHE_NAMESPACE`, Firecracker paths and jailer settings, CNI paths/profile, blocked CIDR lists, `HELMR_WORKER_PROVIDER_REGION`, `HELMR_WORKER_LABELS`, `HELMR_VM_VCPUS`, `HELMR_VM_MEMORY_MIB`, `HELMR_WORKER_DISK_MIB`, and `HELMR_VM_HEALTH_TIMEOUT`. `HELMR_WORKER_LABELS` is a comma-separated `key=value` list used for placement matching. `HELMR_WORKER_DISK_MIB` overrides the filesystem capacity advertised by filesystem-first worker instances.
