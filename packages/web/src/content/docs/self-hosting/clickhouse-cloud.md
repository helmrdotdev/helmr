---
title: ClickHouse Cloud
description: Connect self-hosted Helmr to customer-owned ClickHouse Cloud.
section: Self-hosting
sidebarLabel: ClickHouse Cloud
order: 735
---

# ClickHouse Cloud

Self-hosted Helmr uses ClickHouse Cloud as the historical telemetry store for run logs, events, traces, terminal output, and telemetry analytics. PostgreSQL remains the control database for coordination, cursors, idempotency, watermarks, hot buffers, and run state.

For production self-hosting, use a ClickHouse Cloud service owned by your organization. Local development can use a disposable ClickHouse container, but production should not depend on PostgreSQL-only historical telemetry.

## AWS PrivateLink

The AWS Terraform module `infra/aws/modules/clickhouse-cloud` creates a ClickHouse Cloud service and connects it to your AWS VPC with AWS PrivateLink. The module:

- creates the ClickHouse Cloud service with public IP access disabled;
- creates an AWS interface VPC endpoint in your private subnets;
- creates a narrow private DNS zone for the service private hostname;
- stores the ClickHouse password in AWS Secrets Manager;
- outputs the values consumed by `infra/aws/modules/control`.

Configure the ClickHouse provider in the Terraform root that uses the module. Keep the ClickHouse Cloud API key and secret in your local shell or CI secret store, not in `terraform.tfvars`.

```hcl
terraform {
  required_providers {
    clickhouse = {
      source  = "ClickHouse/clickhouse"
      version = ">= 3.17.3"
    }
  }
}

provider "clickhouse" {
  organization_id = var.clickhouse_organization_id
}
```

The provider reads API credentials from environment variables supported by the provider. Do not commit those values.

Compose the ClickHouse module beside the AWS network and control modules:

```hcl
module "clickhouse" {
  source = "../modules/clickhouse-cloud"

  name              = var.name
  vpc_id            = module.network.vpc_id
  subnet_ids        = module.network.private_subnet_ids
  secret_kms_key_id = var.clickhouse_secret_kms_key_arn
  tags              = local.tags
}

module "control" {
  source = "../modules/control"

  # Existing control inputs omitted.
  clickhouse_url                   = module.clickhouse.clickhouse_url
  clickhouse_user                  = module.clickhouse.clickhouse_user
  clickhouse_password_secret_arn   = module.clickhouse.clickhouse_password_secret_arn
  clickhouse_password_kms_key_arns = var.clickhouse_secret_kms_key_arn == null ? [] : [
    var.clickhouse_secret_kms_key_arn,
  ]

  additional_control_security_group_ids = [
    module.clickhouse.client_security_group_id,
  ]
}
```

The module uses PrivateLink only. If you intentionally use a public ClickHouse endpoint, do not use this module; pass `clickhouse_url`, `clickhouse_user`, and `clickhouse_password_secret_arn` to the control module directly.

## State and Secrets

Terraform state for this module contains generated ClickHouse credential material because Terraform creates both the ClickHouse service password and the AWS Secrets Manager secret version. Store state in an encrypted backend with tightly scoped access.

Do not output or copy the password. Use the emitted `clickhouse_password_secret_arn` so ECS injects `HELMR_CLICKHOUSE_PASSWORD` from Secrets Manager.

If the secret uses a customer-managed KMS key, pass that key ARN to the control module through `clickhouse_password_kms_key_arns` so the control, dispatcher, and migration task execution roles can read the secret.

The module configures the ClickHouse Cloud `default` user because ClickHouse Cloud service creation exposes the initial service password at that boundary. Keep the generated credential scoped to Helmr telemetry and prefer a dedicated ClickHouse service for Helmr. If you need a custom least-privilege ClickHouse user, manage that user after service creation and pass its secret ARN directly to the control module instead of using this module's generated password.

For ephemeral test stacks, set `secret_recovery_window_in_days = 0` so a destroy and re-apply can recreate the same ClickHouse password secret name. Production stacks should keep the default recovery window.

The module targets current ClickHouse Cloud organizations that do not require the legacy `tier` argument. If your ClickHouse organization still requires legacy tiers, create the service separately and pass the resulting endpoint and secret ARN to the control module.

## Apply Order

Create ClickHouse Cloud and the base AWS infrastructure before starting `helmr-control` or `helmr-dispatcher`.

1. Configure the ClickHouse provider with your customer-owned ClickHouse Cloud organization.
2. Apply with `create_control_service = false`.
3. Populate the other Helmr Secrets Manager values.
4. Run database and ClickHouse migrations.
5. Start the control and dispatcher services.

ClickHouse Cloud PrivateLink requires a ClickHouse plan and AWS region that support private endpoints. The module enforces same-region connectivity with the Helmr VPC; use a separate regional stack for cross-region designs.
