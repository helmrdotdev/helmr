---
title: ClickHouse Cloud
description: Connect self-hosted Helmr to customer-owned ClickHouse Cloud.
section: Self-hosting
sidebarLabel: ClickHouse Cloud
order: 735
---

# ClickHouse Cloud

Self-hosted Helmr uses ClickHouse as the historical telemetry store for run
logs, events, traces, terminal output, and analytics. PostgreSQL remains the
Control database for coordination and run state.

Provision a ClickHouse service in your deployment infrastructure, then pass its
connection values to the generic Control module:

```hcl
module "control" {
  source = "../modules/control"

  # Existing Control inputs omitted.
  clickhouse_url                 = var.clickhouse_url
  clickhouse_user                = var.clickhouse_user
  clickhouse_password_secret_arn = var.clickhouse_password_secret_arn

  clickhouse_password_kms_key_arns = var.clickhouse_password_kms_key_arn == null ? [] : [
    var.clickhouse_password_kms_key_arn,
  ]

  additional_control_security_group_ids = var.clickhouse_client_security_group_ids
}
```

Store the password in AWS Secrets Manager and keep provider credentials and
secret values outside Terraform variables. If the password secret uses a
customer-managed KMS key, pass its ARN so Control, Dispatcher, and migration
task execution roles can decrypt it.

PrivateLink, public endpoint policy, DNS, service creation, and ClickHouse
capacity are operator-owned deployment choices. Helmr does not require or
embed a particular ClickHouse provisioning module. Create ClickHouse and its
network path before starting Control or Dispatcher, then run both PostgreSQL
and ClickHouse migrations.
