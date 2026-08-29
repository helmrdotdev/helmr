---
title: Environment variables
description: Supported environment configuration for Helmr services and the CLI.
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
- Established ecosystem variables such as `AWS_*`, `HTTP_PROXY`, and
  `NO_PROXY` keep their standard names.

Environment variable names are an exact, case-sensitive contract. Helmr does
not read legacy aliases. Text, numeric, boolean, and duration settings ignore
surrounding whitespace. Passwords and tokens are opaque and are read exactly as
provided. Base64 root keys must be canonical without surrounding whitespace.
This page covers supported operator and client configuration. Variables owned
entirely by the guest image, tests, or development tooling remain internal.

## CLI

| Variable | Purpose |
| --- | --- |
| `HELMR_API_URL` | Control-plane base URL. |
| `HELMR_API_KEY` | Bearer token for CLI or `HelmrClient`. |
| `HELMR_CONFIG_DIR` | CLI state directory override. |

`HelmrClient` does not read these variables automatically. Pass the URL and API
key from your application's configuration to its constructor.

## Control Plane

Required: `DATABASE_URL`, `CAS_URI`, `CLICKHOUSE_URL`,
`DEPLOYMENT_RUNTIME_DESCRIPTOR_PATH`, `PLATFORM_STORE_URI`,
`WORKER_TOKEN_SIGNING_KEY`, `AUTH_KEY`, `ENCRYPTION_KEY`,
`WORKSPACE_FENCING_KEY`, `TOKEN_CREDENTIAL_KEY`,
`GITHUB_OAUTH_CLIENT_ID`, and `GITHUB_OAUTH_CLIENT_SECRET`.

Deployment mode: `DEPLOYMENT_MODE` defaults to `self-hosted`. In `self-hosted` mode, `SETUP_TOKEN` is required to create the first and only organization. In `managed-cloud` mode, authenticated users can create organizations without a setup token.

Regions and Worker Groups are PostgreSQL resources managed through the Admin
API and Console. Control Plane startup does not reconcile them from process
configuration, and a deployment may start with neither resource.

An optional startup bootstrap seeds one Region and one execution Worker Group.
Set `BOOTSTRAP_ENABLED=true`. When the named Worker
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

`CAPACITY_TOKEN` enables and authenticates the capacity API used by trusted
deployment automation, including an operator-supplied scaler. It must be
canonical unpadded base64url that decodes to exactly 32 bytes. The scaler sends
the same value as a Bearer token over HTTPS outside localhost. Possession grants
the complete capacity API for the deployment, so store it as a secret and
provide it only to the Control Plane and trusted scaler. When it is unset,
capacity API requests are rejected. See [custom capacity
scaling](/docs/self-hosting/capacity-scaling/) for setup and rotation.

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

### Database bootstrap command

`control-plane database-bootstrap [reset]` uses a separate administrative
connection to create or reset the application database role. These variables
are command inputs; the long-running Control Plane does not read the
administrative credentials.

| Variable | Purpose |
| --- | --- |
| `DATABASE_ADMIN_HOST` | Required administrative PostgreSQL host. |
| `DATABASE_ADMIN_PORT` | Administrative PostgreSQL port. Defaults to `5432`. |
| `DATABASE_ADMIN_USERNAME` | Required administrative role. It must differ from the application role in `DATABASE_URL`. |
| `DATABASE_ADMIN_PASSWORD` | Required administrative role password. |
| `DATABASE_NAME` | Required database name. `DATABASE_URL` must target the same name. |
| `DATABASE_URL` | Required application connection URL. It must target the administrative endpoint with TLS, and include the application role and password. |

## Dispatcher

Required: `DATABASE_URL`, `CLICKHOUSE_URL`, and `WORKSPACE_FENCING_KEY`.

`ENCRYPTION_KEY` is control-plane authority and is not provided to the dispatcher.

The dispatcher uses the same single base64-encoded 32-byte
`WORKSPACE_FENCING_KEY` as the Control Plane service.

The AWS Control Plane module provisions cluster-mode disabled ElastiCache Valkey/Redis for the
Control Plane event stream and injects `REDIS_URL` into the Control Plane service.

## Worker

Required for every Worker: `CONTROL_PLANE_URL`, `CAS_URI`,
`PLATFORM_STORE_URI`, `WORKER_RESOURCE_ID`,
`WORKER_POOL_NAME`, `WORKER_ENROLLMENT_TOKEN_FILE`,
`CHECKPOINT_ENCRYPTION_KEY`, `JAILER_UID`, `JAILER_GID`,
`WORKER_NETWORK_LINK_POOL`, `WORKER_NETWORK_TRANSLATION_POOL`,
`WORKER_NETWORK_RESOLVER_IPV4`, and
`WORKER_NETWORK_BLOCKED_IPV4_CIDRS`. The blocked-prefix value is a canonical,
ordered JSON array; use `[]` only when the deployment intentionally supplies no
blocked destinations.

The Worker reads its Worker Group enrollment token from the strict-permission
token file and presents it as a Bearer credential over TLS. The token selects
the Worker Group; the Worker does not configure a group ID. Control Plane
validates the Pool enrollment against that group, records token use, creates
the authoritative Worker-instance identity, and issues a renewable
per-instance credential stored at `WORKER_INSTANCE_CREDENTIAL_PATH`.
`WORKER_RESOURCE_ID` remains an opaque deployment-owned locator for the
physical Worker. Provider identity and infrastructure inventory are deployment
responsibilities rather than Control Plane authentication inputs.

`WORKER_WORK_DIR` and `WORKER_IMAGES_DIR` select Worker state and image
directories. The following advanced settings connect the Worker to binaries
and host layout supplied by its deployment image:

| Variable | Default | Purpose |
| --- | --- | --- |
| `FIRECRACKER_PATH` | `firecracker` | Firecracker binary. |
| `CPU_TEMPLATE_HELPER_PATH` | `cpu-template-helper` | Firecracker CPU-template helper binary. |
| `JAILER_PATH` | `jailer` | Firecracker jailer binary. |
| `MKFS_EXT4_PATH` | `/usr/local/libexec/helmr/mkfs.ext4` | ext4 formatter used for VM disks. |
| `MKE2FS_CONFIG_PATH` | `/usr/share/helmr/mke2fs.conf` | Configuration passed to the ext4 formatter. |
| `IP_PATH` | `ip` | `iproute2` binary used for VM networking. |
| `NFT_PATH` | `nft` | nftables binary used for VM networking. |
| `JAILER_CHROOT_DIR` | Derived from the Worker VM state directory. | Base directory for jailer chroots. |
| `JAILER_CGROUP_VERSION` | `2` | Cgroup version passed to the jailer. |

VM sizing, Worker capacity, disk budgeting, and concurrency use these settings:

| Variable | Default | Purpose |
| --- | --- | --- |
| `VM_VCPUS` | `2` | vCPUs assigned to each VM. |
| `VM_MEMORY_MIB` | `2048` | Memory assigned to each VM. |
| `VM_SCRATCH_DISK_MIB` | `8192` | Scratch disk assigned to each VM. |
| `WORKER_CAPACITY_VCPUS` | `VM_VCPUS` | Total vCPU capacity advertised by the Worker. It cannot be smaller than `VM_VCPUS`. |
| `WORKER_CAPACITY_MEMORY_MIB` | `VM_MEMORY_MIB` | Total memory capacity advertised by the Worker. It cannot be smaller than `VM_MEMORY_MIB`. |
| `WORKER_DISK_MIB` | Total capacity of the Worker filesystem. | Overrides the physical capacity used for disk budgeting. |
| `WORKER_DISK_RESERVE_MIB` | `1024` | Host disk space subtracted to form the physical disk budget. |
| `WORKER_SUBSTRATE_CACHE_MAX_MIB` | Derived when `0`. | Maximum substrate-cache size. |
| `WORKER_ARTIFACT_CACHE_MAX_MIB` | Derived when `0`. | Maximum artifact-cache size. |
| `WORKER_EXECUTION_SLOTS` | `1` | Maximum concurrent executions admitted by the Worker. |
| `VM_INIT_TIMEOUT` | `30s` | Timeout for Firecracker SDK initialization. |
| `VM_HEALTH_TIMEOUT` | `30s` | Timeout for guest health convergence. |

When both cache limits are `0`, the Worker derives one shared cache budget and
splits it two-thirds to the substrate cache and one-third to the artifact
cache. When only one limit is `0`, that cache receives its independently
derived budget. The Worker advertises aggregate guest disk capacity after
subtracting both the reserve and cache budgets from the configured or measured
filesystem total.

The routed-network link and translation pools, resolver, and blocked CIDRs are
required inputs listed above. The AWS Worker profile sets
`VM_HEALTH_TIMEOUT=300s` for first-boot convergence on EC2; that is a provider
profile value, not the Worker process default.
