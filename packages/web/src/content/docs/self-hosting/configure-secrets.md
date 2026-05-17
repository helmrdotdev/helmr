---
title: Configure secrets
description: Populate AWS Secrets Manager values required by the control plane and workers.
section: Self-hosting
sidebarLabel: Configure secrets
order: 740
---

# Configure secrets

The AWS stack creates Secrets Manager entries and returns their ARNs:

```sh
tofu output -json secret_arns
```

Do not place secret values in `terraform.tfvars` or Terraform state.

Populate these secrets after the first apply:

| Secret | Value |
| --- | --- |
| `database_url` | PostgreSQL connection string for the Helmr database. |
| `github_app_private_key` | Raw GitHub App private key PEM. |
| `github_app_webhook_secret` | GitHub App webhook secret. |
| `github_app_client_secret` | GitHub App OAuth client secret. |

The stack generates the remaining internal secrets for auth, worker registration, worker token signing, secret encryption, and checkpoint encryption.

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

Store the private key as the raw PEM text. Preserve its line breaks.
