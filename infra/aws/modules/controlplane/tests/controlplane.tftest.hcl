mock_provider "aws" {
  mock_data "aws_region" { defaults = { region = "us-east-1" } }
  mock_data "aws_caller_identity" { defaults = { account_id = "000000000000" } }
  mock_data "aws_partition" { defaults = { partition = "aws", dns_suffix = "amazonaws.com" } }
  mock_resource "aws_secretsmanager_secret" { defaults = { arn = "arn:aws:secretsmanager:us-east-1:000000000000:secret:mock" } }
  mock_resource "aws_s3_bucket" { defaults = { arn = "arn:aws:s3:::mock-bucket" } }
  mock_resource "aws_kms_key" { defaults = { arn = "arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000000" } }
  mock_resource "aws_iam_role" { defaults = { arn = "arn:aws:iam::000000000000:role/mock" } }
  mock_resource "aws_iam_policy" { defaults = { arn = "arn:aws:iam::000000000000:policy/mock" } }
  mock_resource "aws_elasticache_replication_group" { defaults = { primary_endpoint_address = "redis.example.test" } }
}

mock_provider "random" {}

override_resource {
  target = aws_kms_key.helmr
  values = { arn = "arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000000" }
}

override_resource {
  target = aws_lb.controlplane
  values = { arn = "arn:aws:elasticloadbalancing:us-east-1:000000000000:loadbalancer/app/helmr-test/0000000000000000" }
}

override_resource {
  target = aws_lb_target_group.controlplane
  values = { arn = "arn:aws:elasticloadbalancing:us-east-1:000000000000:targetgroup/helmr-test/0000000000000000" }
}

override_resource {
  target = aws_iam_role.controlplane_execution
  values = { arn = "arn:aws:iam::000000000000:role/helmr-test-controlplane-execution" }
}

override_resource {
  target = aws_iam_role.dispatcher_execution
  values = { arn = "arn:aws:iam::000000000000:role/helmr-test-dispatcher-execution" }
}

override_resource {
  target = aws_iam_role.database_bootstrap_execution
  values = { arn = "arn:aws:iam::000000000000:role/helmr-test-database-bootstrap-execution" }
}

override_resource {
  target = aws_iam_role.controlplane_task
  values = { arn = "arn:aws:iam::000000000000:role/helmr-test-controlplane-task" }
}

override_resource {
  target = aws_iam_role.dispatcher_task
  values = { arn = "arn:aws:iam::000000000000:role/helmr-test-dispatcher-task" }
}

override_resource {
  target = aws_iam_role.migration_task
  values = { arn = "arn:aws:iam::000000000000:role/helmr-test-migration-task" }
}

override_resource {
  target = aws_db_instance.postgres
  values = {
    address = "database.example.test"
    port    = 5432
    master_user_secret = [{
      kms_key_id    = "arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000000"
      secret_arn    = "arn:aws:secretsmanager:us-east-1:000000000000:secret:database-master"
      secret_status = "active"
    }]
  }
}

variables {
  name                                     = "helmr-test"
  vpc_id                                   = "vpc-0123456789abcdef0"
  private_subnet_ids                       = ["subnet-0123456789abcdef0", "subnet-1123456789abcdef0"]
  public_subnet_ids                        = ["subnet-2123456789abcdef0", "subnet-3123456789abcdef0"]
  public_url                               = "http://controlplane.example.test"
  allow_insecure_http                      = true
  bootstrap_worker_group_name              = "default"
  bootstrap_region_id                      = "helmr-us-east"
  controlplane_image                       = "example.invalid/helmr@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  controlplane_image_repository_arn        = "arn:aws:ecr:us-east-1:000000000000:repository/helmr-test/controlplane-releases"
  platform_store_uri                       = "s3://helmr-test-runtime/objects"
  platform_store_bucket_arn                = "arn:aws:s3:::helmr-test-runtime"
  platform_store_kms_key_arn               = "arn:aws:kms:us-east-1:000000000000:key/11111111-1111-1111-1111-111111111111"
  clickhouse_url                           = "https://clickhouse.example.invalid"
  clickhouse_access_mode                   = "external"
  clickhouse_reader_user                   = "telemetry_reader"
  clickhouse_reader_password_secret_arn    = "arn:aws:secretsmanager:us-east-1:000000000000:secret:telemetry-reader"
  clickhouse_ingester_user                 = "telemetry_ingester"
  clickhouse_ingester_password_secret_arn  = "arn:aws:secretsmanager:us-east-1:000000000000:secret:telemetry-ingester"
  clickhouse_migration_user                = "telemetry_migration"
  clickhouse_migration_password_secret_arn = "arn:aws:secretsmanager:us-east-1:000000000000:secret:telemetry-migration"
  github_oauth_client_id                   = "test-client"
  database_skip_final_snapshot             = true
}

run "controlplane_uses_execution_only_runtime_authority" {
  command = apply

  assert {
    condition     = contains([for item in jsondecode(aws_ecs_task_definition.dispatcher.container_definitions)[0].secrets : item.name], "ENCRYPTION_KEY")
    error_message = "Scheduled protected Workspace creation requires the existing encryption key in dispatcher."
  }

  assert {
    condition = (
      aws_db_instance.postgres.engine_version == "18" &&
      aws_db_instance.postgres.auto_minor_version_upgrade
    )
    error_message = "Product must own PostgreSQL major 18 while RDS owns automatic minor upgrades"
  }

  assert {
    condition = (
      length(jsondecode(aws_ecs_task_definition.controlplane.container_definitions)) == 1 &&
      jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].name == "controlplane" &&
      jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].entryPoint == ["/usr/local/bin/control-plane"] &&
      jsondecode(aws_ecs_task_definition.dispatcher.container_definitions)[0].entryPoint == ["dispatcher"] &&
      { for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].environment : item.name => item.value }.PUBLIC_URL == "http://controlplane.example.test" &&
      { for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].environment : item.name => item.value }.PLATFORM_STORE_URI == var.platform_store_uri &&
      aws_lb_target_group.controlplane.health_check[0].path == "/readyz"
    )
    error_message = "Control Plane must start directly with only execution-time Platform Artifact authority"
  }

  assert {
    condition = (
      { for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].environment : item.name => item.value }.BOOTSTRAP_ENABLED == "1" &&
      { for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].environment : item.name => item.value }.BOOTSTRAP_REGION_ID == "helmr-us-east" &&
      { for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].environment : item.name => item.value }.BOOTSTRAP_WORKER_GROUP_NAME == "default" &&
      contains([for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].secrets : item.name], "BOOTSTRAP_WORKER_TOKEN")
    )
    error_message = "Control Plane must receive one explicit Region and execution Worker Group bootstrap"
  }

  assert {
    condition = (
      strcontains(aws_iam_role_policy.controlplane_task.policy, "${var.platform_store_bucket_arn}/objects/sha256/*") &&
      !strcontains(aws_iam_role_policy.controlplane_task.policy, "PublishRetainedArtifacts") &&
      !strcontains(aws_iam_role_policy.controlplane_task.policy, "${var.platform_store_bucket_arn}/controlplane/runtime") &&
      aws_ecs_task_definition.migration.task_role_arn == aws_iam_role.migration_task.arn
    )
    error_message = "Control Plane must read immutable runtime objects without build or rollout authority"
  }

  assert {
    condition = (
      jsondecode(aws_ecs_task_definition.database_bootstrap.container_definitions)[0].command == ["database-bootstrap"] &&
      toset([for item in jsondecode(aws_ecs_task_definition.migration.container_definitions)[0].environment : item.name]) == toset(["CLICKHOUSE_URL", "CLICKHOUSE_USER"])
    )
    error_message = "database bootstrap and migration must keep their narrow one-shot contracts"
  }
}

run "managed_controlplane_omits_setup_token" {
  command = plan
  variables {
    deployment_mode = "managed-cloud"
    api_origin      = "https://api.example.test"
  }
  assert {
    condition = (
      { for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].environment : item.name => item.value }.API_ORIGIN == "https://api.example.test" &&
      !contains([for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].secrets : item.name], "SETUP_TOKEN") &&
      !contains(keys(output.secret_arns), "setup_token")
    )
    error_message = "managed Control Plane must omit the setup token"
  }
}

run "execution_roles_are_pull_only" {
  command = plan
  assert {
    condition = (
      strcontains(aws_iam_role_policy.controlplane_execution.policy, var.controlplane_image_repository_arn) &&
      strcontains(aws_iam_role_policy.dispatcher_execution.policy, var.controlplane_image_repository_arn) &&
      !strcontains(aws_iam_role_policy.controlplane_execution.policy, "ecr:PutImage") &&
      !strcontains(aws_iam_role_policy.dispatcher_execution.policy, "ecr:PutImage")
    )
    error_message = "ECS execution roles must have pull-only access to the exact Control Plane repository"
  }
}

run "capacity_api_credential_is_explicit_composition" {
  command = plan
  variables {
    capacity_token_secret_arn  = "arn:aws:secretsmanager:us-east-1:111122223333:secret:helmr/capacity-token"
    capacity_token_kms_key_arn = "arn:aws:kms:us-east-1:111122223333:key/12345678-1234-1234-1234-123456789012"
  }
  assert {
    condition = (
      contains([for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].secrets : item.name], "CAPACITY_TOKEN") &&
      !contains([for item in jsondecode(aws_ecs_task_definition.dispatcher.container_definitions)[0].secrets : item.name], "CAPACITY_TOKEN")
    )
    error_message = "only Control Plane receives the composed capacity credential"
  }
}

run "capacity_api_credential_rejects_plaintext_environment" {
  command = plan
  variables {
    controlplane_environment = { CAPACITY_TOKEN = "plaintext-must-not-enter-task-definition" }
  }
  expect_failures = [terraform_data.bootstrap_preconditions]
}

run "caller_ceiling_and_roll_forward" {
  command = plan
  variables {
    permissions_boundary_arn    = "arn:aws:iam::000000000000:policy/workload"
    create_controlplane_service = true
    enable_deployment_rollback  = false
  }
  assert {
    condition     = alltrue([for role in [aws_iam_role.controlplane_execution, aws_iam_role.dispatcher_execution, aws_iam_role.database_bootstrap_execution, aws_iam_role.migration_execution, aws_iam_role.controlplane_task, aws_iam_role.dispatcher_task, aws_iam_role.migration_task] : role.permissions_boundary == var.permissions_boundary_arn])
    error_message = "Every Product ECS role must use the caller ceiling."
  }
  assert {
    condition     = alltrue([for service in [aws_ecs_service.controlplane[0], aws_ecs_service.dispatcher[0]] : service.deployment_circuit_breaker[0].enable && !service.deployment_circuit_breaker[0].rollback])
    error_message = "A failed deployment must not restart predecessor code after migrations."
  }
}
run "foreign_ceiling" {
  command = plan
  variables { permissions_boundary_arn = "arn:aws:iam::999999999999:policy/workload" }
  expect_failures = [aws_iam_role.controlplane_execution]
}

run "malformed_ceiling" {
  command = plan
  variables { permissions_boundary_arn = "workload" }
  expect_failures = [var.permissions_boundary_arn]
}

run "default_automatic_recovery" {
  command = plan
  variables { create_controlplane_service = true }
  assert {
    condition     = alltrue([for service in [aws_ecs_service.controlplane[0], aws_ecs_service.dispatcher[0]] : service.deployment_circuit_breaker[0].enable && service.deployment_circuit_breaker[0].rollback])
    error_message = "Both services must enable automatic predecessor recovery by default."
  }
}

run "external_clickhouse_credentials_are_scoped_to_each_task" {
  command = apply
  assert {
    condition = (
      { for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].environment : item.name => item.value }.CLICKHOUSE_USER == var.clickhouse_reader_user &&
      { for item in jsondecode(aws_ecs_task_definition.dispatcher.container_definitions)[0].environment : item.name => item.value }.CLICKHOUSE_USER == var.clickhouse_ingester_user &&
      { for item in jsondecode(aws_ecs_task_definition.migration.container_definitions)[0].environment : item.name => item.value }.CLICKHOUSE_USER == var.clickhouse_migration_user &&
      length(aws_ecs_task_definition.clickhouse_bootstrap) == 0
    )
    error_message = "External mode must wire dedicated identities and omit the administrative bootstrap task."
  }
  assert {
    condition = (
      strcontains(aws_iam_role_policy.controlplane_execution.policy, var.clickhouse_reader_password_secret_arn) &&
      !strcontains(aws_iam_role_policy.controlplane_execution.policy, var.clickhouse_ingester_password_secret_arn) &&
      !strcontains(aws_iam_role_policy.controlplane_execution.policy, var.clickhouse_migration_password_secret_arn) &&
      strcontains(aws_iam_role_policy.dispatcher_execution.policy, var.clickhouse_ingester_password_secret_arn) &&
      !strcontains(aws_iam_role_policy.dispatcher_execution.policy, var.clickhouse_reader_password_secret_arn) &&
      !strcontains(aws_iam_role_policy.dispatcher_execution.policy, var.clickhouse_migration_password_secret_arn) &&
      strcontains(aws_iam_role_policy.migration_execution.policy, var.clickhouse_migration_password_secret_arn) &&
      !strcontains(aws_iam_role_policy.migration_execution.policy, var.clickhouse_reader_password_secret_arn) &&
      !strcontains(aws_iam_role_policy.migration_execution.policy, var.clickhouse_ingester_password_secret_arn) &&
      aws_ecs_task_definition.migration.execution_role_arn == aws_iam_role.migration_execution.arn
    )
    error_message = "Execution roles must not retrieve another task's ClickHouse credential."
  }
}

run "bootstrap_clickhouse_admin_is_confined_to_one_off_task" {
  command = apply
  variables {
    clickhouse_access_mode                   = "bootstrap"
    clickhouse_bootstrap_user                = "default"
    clickhouse_bootstrap_password_secret_arn = "arn:aws:secretsmanager:us-east-1:000000000000:secret:telemetry-admin"
  }
  assert {
    condition = (
      jsondecode(aws_ecs_task_definition.clickhouse_bootstrap[0].container_definitions)[0].command == ["clickhouse-bootstrap"] &&
      length(jsondecode(aws_ecs_task_definition.clickhouse_bootstrap[0].container_definitions)[0].secrets) == 4 &&
      aws_ecs_task_definition.clickhouse_bootstrap[0].execution_role_arn == aws_iam_role.clickhouse_bootstrap_execution[0].arn &&
      coalesce(aws_ecs_task_definition.clickhouse_bootstrap[0].task_role_arn, "none") == "none" &&
      aws_iam_role.clickhouse_bootstrap_execution[0].permissions_boundary == var.permissions_boundary_arn
    )
    error_message = "Bootstrap must use the existing image as a separate one-off task with four credentials and no task role."
  }
  assert {
    condition = (
      strcontains(aws_iam_role_policy.clickhouse_bootstrap_execution[0].policy, var.clickhouse_bootstrap_password_secret_arn) &&
      strcontains(aws_iam_role_policy.clickhouse_bootstrap_execution[0].policy, var.clickhouse_reader_password_secret_arn) &&
      strcontains(aws_iam_role_policy.clickhouse_bootstrap_execution[0].policy, var.clickhouse_ingester_password_secret_arn) &&
      strcontains(aws_iam_role_policy.clickhouse_bootstrap_execution[0].policy, var.clickhouse_migration_password_secret_arn) &&
      !strcontains(aws_iam_role_policy.controlplane_execution.policy, var.clickhouse_bootstrap_password_secret_arn) &&
      !strcontains(aws_iam_role_policy.dispatcher_execution.policy, var.clickhouse_bootstrap_password_secret_arn) &&
      !strcontains(aws_iam_role_policy.migration_execution.policy, var.clickhouse_bootstrap_password_secret_arn)
    )
    error_message = "Only the account-bootstrap execution role may retrieve the administrator secret."
  }
}

run "reject_shared_clickhouse_runtime_identity" {
  command = plan
  variables { clickhouse_reader_user = "telemetry_ingester" }
  expect_failures = [terraform_data.clickhouse_access_preconditions]
}
run "reject_default_clickhouse_runtime_identity" {
  command = plan
  variables { clickhouse_reader_user = "default" }
  expect_failures = [terraform_data.clickhouse_access_preconditions]
}
run "reject_shared_clickhouse_runtime_secret" {
  command = plan
  variables { clickhouse_reader_password_secret_arn = "arn:aws:secretsmanager:us-east-1:000000000000:secret:telemetry-ingester" }
  expect_failures = [terraform_data.clickhouse_access_preconditions]
}
run "reject_bootstrap_without_admin" {
  command = plan
  variables { clickhouse_access_mode = "bootstrap" }
  expect_failures = [terraform_data.clickhouse_access_preconditions]
}

run "self_hosted_computer_root_is_separate_secret" {
  command = apply
  variables { computer_wrapping_key_id = "restored-root-v1" }
  assert {
    condition = (
      { for x in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].environment : x.name => x.value }.COMPUTER_WRAPPING_KEY_ID == "restored-root-v1" &&
      { for x in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].secrets : x.name => x.valueFrom }.COMPUTER_WRAPPING_KEY == output.secret_arns.computer_wrapping_key &&
      aws_secretsmanager_secret.computer_wrapping_key[0].name != aws_secretsmanager_secret.encryption_key.name &&
      !strcontains(aws_iam_role_policy.controlplane_task.policy, "WrapComputerKeys") &&
      !contains([for x in jsondecode(aws_ecs_task_definition.dispatcher.container_definitions)[0].secrets : x.name], "COMPUTER_WRAPPING_KEY")
    )
    error_message = "Self-hosted Computer keys must use a dedicated out-of-band root secret only in Control Plane."
  }
}

run "managed_computer_root_is_context_bound_kms" {
  command = apply
  variables { deployment_mode = "managed-cloud" }
  assert {
    condition = (
      { for x in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].environment : x.name => x.value }.COMPUTER_KMS_KEY_ARN == aws_kms_key.helmr.arn &&
      !contains(keys(output.secret_arns), "computer_wrapping_key") &&
      !contains([for x in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].environment : x.name], "COMPUTER_WRAPPING_KEY_ID")
    )
    error_message = "Managed Control Plane must select the existing KMS key without a local wrapping key."
  }
  assert {
    condition = one([for s in jsondecode(aws_iam_role_policy.controlplane_task.policy).Statement : s if try(s.Sid, "") == "WrapComputerKeys"]) == {
      Sid      = "WrapComputerKeys"
      Effect   = "Allow"
      Action   = ["kms:Encrypt", "kms:Decrypt"]
      Resource = aws_kms_key.helmr.arn
      Condition = {
        StringEquals                = { "kms:EncryptionContext:purpose" = "helmr.computer-key.v1", "kms:EncryptionAlgorithm" = "SYMMETRIC_DEFAULT" }
        StringLike                  = { "kms:EncryptionContext:computer_scope" = "?*", "kms:EncryptionContext:key_id" = "?*" }
        "ForAllValues:StringEquals" = { "kms:EncryptionContextKeys" = ["purpose", "computer_scope", "key_id"] }
        Null                        = { "kms:ViaService" = "true" }
      }
    }
    error_message = "Only exact-key, exact-context Encrypt/Decrypt may be added for Computer wrapping."
  }
  assert {
    condition     = alltrue([for policy in [aws_iam_role_policy.migration_execution.policy, aws_iam_role_policy.controlplane_execution.policy, aws_iam_role_policy.dispatcher_execution.policy] : !strcontains(policy, "helmr.computer-key.v1")])
    error_message = "Other roles must not gain direct Computer root authority."
  }
}

run "reject_computer_root_override" {
  command = plan
  variables { controlplane_environment = { COMPUTER_KMS_KEY_ARN = "other" } }
  expect_failures = [terraform_data.bootstrap_preconditions]
}
