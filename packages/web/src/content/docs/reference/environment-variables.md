---
title: Environment variables
description: Environment variables read by Helmr services and the CLI.
sidebarLabel: Environment variables
---

# Environment variables

Helmr names environment variables according to the environment that owns them:

- A dedicated Helmr process uses direct configuration names such as
  `DATABASE_URL`, `CONTROL_PLANE_ADDR`, and `WORKER_RESOURCE_ID`. Repeating
  `HELMR_` inside a process-specific environment adds no ownership information.
- `HELMR_` is reserved for public client configuration and values that cross
  into an environment Helmr does not exclusively own, such as a user's shell
  or a task guest. The CLI variables below are examples.
- Established ecosystem variables such as `AWS_*`, `OTEL_*`, `HTTP_PROXY`,
  and `NO_PROXY` keep their standard names.

Environment variable names are an exact, case-sensitive contract. Helmr does
not read legacy aliases. Text, numeric, boolean, and duration settings ignore
surrounding whitespace. Passwords and tokens are opaque and are read exactly as
provided. Base64 root keys must be canonical without surrounding whitespace.

## CLI

| Variable | Purpose |
| --- | --- |
| `HELMR_API_URL` | Control-plane base URL. |
| `HELMR_API_KEY` | Bearer token for CLI or `HelmrClient`. |
| `HELMR_CONFIG_DIR` | CLI state directory override. |

`HelmrClient` does not read these variables automatically. Pass the URL and API
key from your application's configuration to its constructor.

## Control Plane

Required: `DATABASE_URL`, `CAS_URI`, `CLICKHOUSE_URL`, `BUILD_POLICY_PATH`,
`PLATFORM_STORE_URI`, `WORKER_TOKEN_SIGNING_KEY`, `AUTH_KEY`, `ENCRYPTION_KEY`,
`WORKSPACE_FENCING_KEY`, `TOKEN_CREDENTIAL_KEY`,
`GITHUB_OAUTH_CLIENT_ID`, and `GITHUB_OAUTH_CLIENT_SECRET`.

Deployment mode: `DEPLOYMENT_MODE` defaults to `self-hosted`. In `self-hosted` mode, `SETUP_TOKEN` is required to create the first and only organization. In `managed-cloud` mode, authenticated users can create organizations without a setup token.

Regions and Worker Groups are PostgreSQL resources managed through the Admin
API and Console. Control Plane startup does not reconcile them from process
configuration, and a deployment may start with neither resource.

An optional startup bootstrap seeds one Region and one combined run/build
Worker Group. Set `BOOTSTRAP_ENABLED=true`. When the named Worker
Group does not exist, `BOOTSTRAP_WORKER_TOKEN` is also required.
`BOOTSTRAP_REGION_ID` and
`BOOTSTRAP_WORKER_GROUP_NAME` default to `default`.
`BOOTSTRAP_REGION_DISPLAY_NAME` and `BOOTSTRAP_REGION_LOCATION` are optional.
Bootstrap creates missing resources and never updates an existing Region or
Worker Group. Changing either bootstrap identity can therefore create another
missing seed; it does not rename or delete an existing resource. When needed,
the token must use the `hlmr_wgt_` format; only its SHA-256 hash is stored in
PostgreSQL.

`BOOTSTRAP_REGION_ID` is an opaque Helmr identifier, not a provider Region or
DNS name. Its normalized UTF-8 value must be 1–255 bytes and contain no
surrounding whitespace or control characters.

`ADMIN_EMAILS` is an optional comma-separated list of normalized user email
addresses that receive the platform-wide Admin flag when their user record is
created. Admin differs from organization membership roles and controls the
`/admin` Console and `/admin/api/v1` API surfaces.

Optional: `CONTROL_PLANE_ADDR`, `PUBLIC_URL`, `API_ORIGIN`, `REDIS_URL`, and
`MAGIC_LINK_DEBUG_URLS`. `PUBLIC_URL` is used for browser-facing links.
`API_ORIGIN` is used for machine-facing token callback URLs and defaults to
`PUBLIC_URL`. `REDIS_URL` defaults to
`redis://127.0.0.1:6379/0`.

ClickHouse telemetry: `CLICKHOUSE_URL` is required. Set `CLICKHOUSE_USER` when the service user is not `default`, and set `CLICKHOUSE_PASSWORD` when the service requires a password.

`AUTH_KEY`, `TOKEN_CREDENTIAL_KEY`, `WORKSPACE_FENCING_KEY`,
`ENCRYPTION_KEY`, and `WORKER_TOKEN_SIGNING_KEY` are distinct single roots.
Each must be base64 and decode to exactly 32 bytes. Every Control Plane replica uses
the same values. Online rotation and multi-key verification are not supported.

Email delivery is disabled by default. Set `EMAIL_PROVIDER` to choose a sender:

| Provider | Required variables | Optional variables |
| --- | --- | --- |
| `none` | None. This is the default when no email settings are present. | None |
| `log` | `EMAIL_PROVIDER=log` | `MAGIC_LINK_DEBUG_URLS=true` logs magic link URLs for local debugging. |
| `smtp` | `EMAIL_PROVIDER=smtp`, `SMTP_ADDR`, `EMAIL_FROM` | `SMTP_USERNAME`, `SMTP_PASSWORD` |
| `resend` | `EMAIL_PROVIDER=resend`, `RESEND_API_KEY`, `EMAIL_FROM` | None |

`EMAIL_FROM` must be an email address or display-name address accepted by the selected provider, such as `Helmr <noreply@example.com>`.

## Dispatcher

Required: `DATABASE_URL`, `CLICKHOUSE_URL`, and `WORKSPACE_FENCING_KEY`.
`REDIS_URL` defaults to `redis://127.0.0.1:6379/0`.

`ENCRYPTION_KEY` is control-plane authority and is not provided to the dispatcher.

The dispatcher uses the same single base64-encoded 32-byte
`WORKSPACE_FENCING_KEY` as the Control Plane service.

The AWS Control Plane module provisions cluster-mode disabled ElastiCache Valkey/Redis and injects
`REDIS_URL` into both `helmr-controlplane` and `helmr-dispatcher`.

Optional Run placement tuning:

| Variable | Default | Purpose |
| --- | --- | --- |
| `RUN_RESERVATION_TTL` | `5m` | Lifetime of a cold runtime reservation before fenced cleanup is required. |
| `RUN_LEASE_START_DEADLINE` | `1m` | Time allowed for a worker to claim a newly assigned Run Lease. |
| `RUN_LEASE_TTL` | `5m` | Operational lifetime sampled when a Run and Workspace Lease are granted or renewed. Must be at least the start deadline. |

Optional Schedule worker tuning:

| Variable | Default | Purpose |
| --- | --- | --- |
| `SCHEDULE_POLL_INTERVAL` | `1s` | How often the dispatcher claims due Schedule cursors from PostgreSQL. |
| `SCHEDULE_CLAIM_LIMIT` | `100` | Maximum due Schedule rows claimed per poll; must be an integer from 1 through 2147483647. |
| `SCHEDULE_CONCURRENCY` | `10` | Maximum concurrent Schedule admission transactions per dispatcher; must be an integer from 1 through 2147483647. |
| `SCHEDULE_CLAIM_LEASE` | `5m` | PostgreSQL claim lease held while one Schedule cursor is admitted. |

## Worker

Required for every Worker: `CONTROL_PLANE_URL`, `CAS_URI`,
`PLATFORM_STORE_URI`, `WORKER_RESOURCE_ID`,
`WORKER_ENROLLMENT_TOKEN_FILE`, `WORKER_ROLES`,
`CHECKPOINT_ENCRYPTION_KEY`, `JAILER_UID`, `JAILER_GID`,
`WORKER_NETWORK_LINK_POOL`, `WORKER_NETWORK_TRANSLATION_POOL`,
`WORKER_NETWORK_RESOLVER_IPV4`, and
`WORKER_NETWORK_BLOCKED_IPV4_CIDRS`. The blocked-prefix value is a canonical,
ordered JSON array; use `[]` only when the deployment intentionally supplies no
blocked destinations.

A Worker with the `build` role also requires `BUILD_POLICY_PATH`,
`WORKER_BUILD_CACHE_DIR`, `WORKER_BUILD_SCRATCH_DIR`, positive
`WORKER_SUBSTRATE_CACHE_MAX_MIB`, and positive
`WORKER_ARTIFACT_CACHE_MAX_MIB`.

The Worker reads its Worker Group enrollment token from the strict-permission
token file and presents it as a Bearer credential over TLS. The token selects
the Worker Group; the Worker does not configure a group ID. Control Plane
validates the requested roles against that group, records token use, creates
the authoritative Worker-instance identity, and issues a renewable
per-instance credential stored at `WORKER_INSTANCE_CREDENTIAL_PATH`.
`WORKER_RESOURCE_ID` remains an opaque deployment-owned locator for the
physical Worker. Provider identity and infrastructure inventory are deployment
responsibilities rather than Control Plane authentication inputs.

Runtime inputs include `WORKER_WORK_DIR`, `WORKER_IMAGES_DIR`, Firecracker paths and jailer settings, routed-network link and translation pools, resolver and blocked CIDRs, `VM_VCPUS`, `VM_MEMORY_MIB`, `WORKER_DISK_MIB`, and `VM_HEALTH_TIMEOUT`. `WORKER_DISK_MIB` overrides the filesystem capacity advertised by filesystem-first worker instances. Workspace-image builds start the pinned BuildKit daemon inside a fresh image-build guest; there is no host BuildKit address or service setting.
