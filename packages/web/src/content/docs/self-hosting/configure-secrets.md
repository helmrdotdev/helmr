---
title: Configure secrets
description: Populate AWS Secrets Manager values required by the Control Plane and workers.
section: Self-hosting
sidebarLabel: Configure secrets
order: 740
---

# Configure secrets

The AWS stack creates Secrets Manager entries and returns their ARNs:

```sh
tofu output -json secret_arns
```

Do not place Helmr application secret values in `terraform.tfvars` or Terraform state. Provision the ClickHouse password in your deployment secret manager and pass only its ARN to the Control Plane module.

Populate these secrets after the first apply:

| Secret | Value |
| --- | --- |
| `database_url` | PostgreSQL connection string for the Helmr database. |
| `worker_token_signing_key` | Base64-encoded 32-byte signing key. |
| `auth_key` | Base64-encoded 32-byte authentication root. |
| `encryption_key` | Base64-encoded 32-byte key used for Secret values. |
| `workspace_fencing_key` | Base64-encoded 32-byte Workspace fencing root. |
| `token_credential_key` | Base64-encoded 32-byte Token credential root. |
| `checkpoint_encryption_key` | Base64-encoded 32-byte key. |
| `setup_token` | High-entropy token for first organization setup. |
| `github_oauth_client_secret` | GitHub OAuth client secret. |

Create a Secrets Manager secret for `CLICKHOUSE_PASSWORD` and pass its ARN as `clickhouse_password_secret_arn`. If that secret uses a customer-managed KMS key outside the stack, also pass the key ARN through `clickhouse_password_kms_key_arns`.

The Terraform/OpenTofu stack creates empty Secrets Manager entries. It does not generate Helmr
internal secret values. Use the bootstrap helper from the AWS profile directory to generate the
internal values locally and write them directly to Secrets Manager:

```sh
../../../scripts/aws-bootstrap-helmr-secrets.sh
```

Set these environment variables to populate application secrets in the same run:

- `HELMR_DATABASE_URL`
- `HELMR_GITHUB_OAUTH_CLIENT_SECRET`

The helper uses `tofu output -json secret_arns` by default. Set `TOFU=terraform` when using
Terraform. The helper initializes missing values only and never replaces an existing value.
Rotate a Worker Group enrollment token from Admin and propagate it to Workers explicitly.
Online rotation of root signing and encryption keys is not supported.

Use the RDS endpoint output when building the database URL:

```sh
tofu output postgres_endpoint
```

Read the generated RDS master password from the RDS-managed secret:

```sh
aws secretsmanager get-secret-value \
  --secret-id "$(tofu output -raw database_master_user_secret_arn)" \
  --query SecretString \
  --output text | jq -r '.password'
```

The format is:

```text
postgres://helmr:<password>@<postgres_endpoint>/helmr?sslmode=require
```

Write a value with AWS CLI:

```sh
aws secretsmanager put-secret-value \
  --secret-id <secret_arn> \
  --secret-string '<secret_value>'
```

When `email_provider = "resend"` is set, the stack creates a `secret_arns.resend_api_key`
Secrets Manager secret. Populate it with the raw Resend API key before starting the Control Plane
service.

When `email_provider = "smtp"` and `smtp_password_enabled = true` are set, the stack creates a
`secret_arns.smtp_password` Secrets Manager secret. Populate it with the raw SMTP password before
starting the Control Plane service.
