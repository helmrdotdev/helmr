---
title: Secrets and data
description: Connect PostgreSQL and ClickHouse, populate Secrets Manager, and preserve encryption keys.
sidebarLabel: Secrets and data
---

# Secrets and data

The AWS compositions create empty Secrets Manager containers. They do not generate application secret values in Terraform or place those values in state.

## PostgreSQL

RDS creates a managed master credential. Use it only for the one-off database bootstrap task. Control Plane, dispatcher, and migrations use a static, non-administrative application URL stored in `secret_arns.database_url`:

```text
postgres://helmr_app:<application-password>@<postgres_endpoint>/helmr?sslmode=require
```

Choose and manage the `helmr_app` password outside Terraform, then supply the complete URL to the secret bootstrap helper.

## ClickHouse

ClickHouse stores historical telemetry. Provision the service and network path outside these example compositions, then configure its HTTPS endpoint and optional username in tfvars. Store its password in a separate Secrets Manager secret and pass only `clickhouse_password_secret_arn`.

If that secret uses a customer-managed KMS key, include it in `clickhouse_password_kms_key_arns`. Attach any required ClickHouse client security groups through `additional_controlplane_security_group_ids`. PrivateLink, public-endpoint policy, DNS, capacity, backups, and service creation remain operator-owned.

## Populate application secrets

Inspect the created destinations:

```sh
tofu output -json secret_arns
tofu output -raw worker_enrollment_secret_arn
```

From the selected profile directory, run:

```sh
export HELMR_DATABASE_URL='postgres://helmr_app:...@.../helmr?sslmode=require'
export HELMR_GITHUB_OAUTH_CLIENT_SECRET='...'
../../../scripts/aws-bootstrap-helmr-secrets.sh
```

Set `TOFU=terraform` if required. The helper creates locally generated values for:

- `worker_token_signing_key`
- `auth_key`
- `encryption_key`
- `workspace_fencing_key`
- `token_credential_key`
- `checkpoint_encryption_key`
- `setup_token`
- the worker-group enrollment token

The six root keys are base64-encoded 32-byte values. The helper writes only secrets that do not already have an `AWSCURRENT` value; it never overwrites existing values.

Optional email providers create additional secret destinations. Resend needs `email_provider = "resend"`, `email_from`, and a raw API key in `secret_arns.resend_api_key`. SMTP needs `email_provider = "smtp"`, `smtp_addr`, `email_from`, and, when enabled, a raw password in `secret_arns.smtp_password`.

## Rotation and recovery limits

Rotate a worker-group enrollment token from Admin and propagate it to the group explicitly. Online rotation of the root signing and encryption keys is not supported by the checked-in deployment flow. Preserve the checkpoint encryption key for every worker that may restore existing checkpoints; losing or changing it can make encrypted checkpoint data unusable.

Keep secret values out of tfvars, command history, logs, and support bundles.
