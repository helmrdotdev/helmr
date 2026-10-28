mock_provider "aws" {
  mock_resource "aws_ecs_cluster" { defaults = { arn = "arn:aws:ecs:us-east-1:000000000000:cluster/mock" } }
  mock_resource "aws_ecs_task_definition" { defaults = { arn = "arn:aws:ecs:us-east-1:000000000000:task-definition/mock:1" } }
  mock_resource "aws_cloudwatch_event_rule" { defaults = { arn = "arn:aws:events:us-east-1:000000000000:rule/mock" } }
  mock_data "aws_region" { defaults = { region = "us-east-1" } }
  mock_data "aws_caller_identity" { defaults = { account_id = "000000000000" } }
  mock_data "aws_partition" { defaults = { partition = "aws", dns_suffix = "amazonaws.com" } }
  mock_resource "aws_secretsmanager_secret" { defaults = { arn = "arn:aws:secretsmanager:us-east-1:000000000000:secret:mock" } }
  mock_resource "aws_s3_bucket" { defaults = { arn = "arn:aws:s3:::mock-bucket" } }
  mock_resource "aws_kms_key" { defaults = { arn = "arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000000" } }
  mock_resource "aws_iam_role" { defaults = { arn = "arn:aws:iam::000000000000:role/mock" } }
  mock_resource "aws_iam_policy" { defaults = { arn = "arn:aws:iam::000000000000:policy/mock" } }
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
  quarantine_upload_vpc_endpoint_id = "vpce-0123456789abcdef0"
  audit_retention_seconds           = 2592000
  controlplane_environment = {
    DEPLOYMENT_UPLOAD_PROCESS_CONCURRENCY      = "8"
    DEPLOYMENT_UPLOAD_ORGANIZATION_CONCURRENCY = "8"
    DEPLOYMENT_UPLOAD_IDLE_TIMEOUT             = "2m"
    DEPLOYMENT_UPLOAD_MAX_DURATION             = "2h"

    ENVIRONMENT_MAX_RESIDENT_COMPUTERS     = "10"
    ENVIRONMENT_MAX_CPU_MILLIS             = "16000"
    ENVIRONMENT_MAX_MEMORY_BYTES           = "68719476736"
    ENVIRONMENT_MAX_RESERVED_STORAGE_BYTES = "1099511627776"
    ENVIRONMENT_MAX_OUTSTANDING_ADMISSIONS = "100"
    ENVIRONMENT_MAX_CAUSAL_DEPTH           = "8"
    ENVIRONMENT_ADMISSION_RATE_PER_SECOND  = "10"
    ENVIRONMENT_ADMISSION_BURST            = "20"
    ENVIRONMENT_PREPARATION_TIMEOUT_MS     = "600000"
    DIAGNOSTIC_CHUNK_BYTES                 = "65536"
    DIAGNOSTIC_SOURCE_BYTES                = "4194304"
    DIAGNOSTIC_SOURCE_RECORDS              = "1024"
    DIAGNOSTIC_ENVIRONMENT_BYTES           = "16777216"
    DIAGNOSTIC_ENVIRONMENT_RECORDS         = "4096"
    DIAGNOSTIC_QUEUE_BYTES                 = "67108864"
    DIAGNOSTIC_QUEUE_RECORDS               = "16384"
    DIAGNOSTIC_DB_MAX_CONNECTIONS          = "2"
  }
  dispatcher_environment = {
    DIAGNOSTIC_EXPORT_BATCH_BYTES   = "4194304"
    DIAGNOSTIC_EXPORT_BATCH_RECORDS = "1024"
    DIAGNOSTIC_CHUNK_BYTES          = "65536"
    DIAGNOSTIC_SOURCE_BYTES         = "4194304"
    DIAGNOSTIC_SOURCE_RECORDS       = "1024"
    DIAGNOSTIC_ENVIRONMENT_BYTES    = "16777216"
    DIAGNOSTIC_ENVIRONMENT_RECORDS  = "4096"
    DIAGNOSTIC_QUEUE_BYTES          = "67108864"
    DIAGNOSTIC_QUEUE_RECORDS        = "16384"
    DIAGNOSTIC_DB_MAX_CONNECTIONS   = "2"
  }
  name                                     = "helmr-test"
  vpc_id                                   = "vpc-0123456789abcdef0"
  private_subnet_ids                       = ["subnet-0123456789abcdef0", "subnet-1123456789abcdef0"]
  public_subnet_ids                        = ["subnet-2123456789abcdef0", "subnet-3123456789abcdef0"]
  app_origin                               = "http://controlplane.example.test"
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
    condition     = { for item in jsondecode(aws_ecs_task_definition.dispatcher.container_definitions)[0].environment : item.name => item.value }.CONTROL_PLANE_URL == output.controlplane_url
    error_message = "Dispatcher fencing must observe the same public Control Plane endpoint as Workers."
  }

  assert {
    condition     = { for item in jsondecode(aws_ecs_task_definition.dispatcher.container_definitions)[0].environment : item.name => item.value }.APP_ORIGIN == output.controlplane_url
    error_message = "Slack publications must link to this installation's Console origin."
  }

  assert {
    condition     = contains([for item in jsondecode(aws_ecs_task_definition.dispatcher.container_definitions)[0].secrets : item.name], "ENCRYPTION_KEY")
    error_message = "Scheduled protected Computer creation requires the existing encryption key in dispatcher."
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
      { for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].environment : item.name => item.value }.APP_ORIGIN == "http://controlplane.example.test" &&
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
  }
  assert {
    condition = (
      !contains([for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].secrets : item.name], "SETUP_TOKEN") &&
      !contains(keys(output.secret_arns), "setup_token")
    )
    error_message = "managed Control Plane must omit the setup token"
  }
}

run "execution_roles_are_pull_only" {
  command = plan
  assert {
    condition = alltrue([
      for policy in [aws_iam_role_policy.controlplane_execution.policy, aws_iam_role_policy.dispatcher_execution.policy] :
      alltrue([for statement in jsondecode(policy).Statement :
        statement.Effect == "Allow" && !can(statement.NotAction) && !can(statement.NotResource) &&
        alltrue([for action in flatten([statement.Action]) : contains(["logs:CreateLogStream", "logs:PutLogEvents", "secretsmanager:GetSecretValue", "kms:Decrypt", "ecr:GetAuthorizationToken", "ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer"], action)])
      ]) &&
      toset([for statement in jsondecode(policy).Statement : jsonencode(merge(statement, { Action = sort(flatten([statement.Action])) })) if anytrue([for action in flatten([statement.Action]) : startswith(action, "ecr:")])]) ==
      toset([
        jsonencode({ Sid = "AuthenticateControlPlaneReleaseRegistry", Effect = "Allow", Action = ["ecr:GetAuthorizationToken"], Resource = "*" }),
        jsonencode({ Sid = "PullControlPlaneReleaseImage", Effect = "Allow", Action = ["ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer"], Resource = var.controlplane_image_repository_arn })
      ])
    ])
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
    controlplane_environment = {
      DEPLOYMENT_UPLOAD_PROCESS_CONCURRENCY      = "8"
      DEPLOYMENT_UPLOAD_ORGANIZATION_CONCURRENCY = "8"
      DEPLOYMENT_UPLOAD_IDLE_TIMEOUT             = "2m"
      DEPLOYMENT_UPLOAD_MAX_DURATION             = "2h"

      ENVIRONMENT_MAX_RESIDENT_COMPUTERS     = "10"
      ENVIRONMENT_MAX_CPU_MILLIS             = "16000"
      ENVIRONMENT_MAX_MEMORY_BYTES           = "68719476736"
      ENVIRONMENT_MAX_RESERVED_STORAGE_BYTES = "1099511627776"
      ENVIRONMENT_MAX_OUTSTANDING_ADMISSIONS = "100"
      ENVIRONMENT_MAX_CAUSAL_DEPTH           = "8"
      ENVIRONMENT_ADMISSION_RATE_PER_SECOND  = "10"
      ENVIRONMENT_ADMISSION_BURST            = "20"
      ENVIRONMENT_PREPARATION_TIMEOUT_MS     = "600000"
      CAPACITY_TOKEN                         = "plaintext-must-not-enter-task-definition"
    }
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

run "reject_shared_clickhouse_vm_platform" {
  command = plan
  variables { clickhouse_reader_user = "telemetry_ingester" }
  expect_failures = [terraform_data.clickhouse_access_preconditions]
}
run "reject_default_clickhouse_vm_platform" {
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
  variables { controlplane_environment = {
    DEPLOYMENT_UPLOAD_PROCESS_CONCURRENCY      = "8"
    DEPLOYMENT_UPLOAD_ORGANIZATION_CONCURRENCY = "8"
    DEPLOYMENT_UPLOAD_IDLE_TIMEOUT             = "2m"
    DEPLOYMENT_UPLOAD_MAX_DURATION             = "2h"

    ENVIRONMENT_MAX_RESIDENT_COMPUTERS     = "10"
    ENVIRONMENT_MAX_CPU_MILLIS             = "16000"
    ENVIRONMENT_MAX_MEMORY_BYTES           = "68719476736"
    ENVIRONMENT_MAX_RESERVED_STORAGE_BYTES = "1099511627776"
    ENVIRONMENT_MAX_OUTSTANDING_ADMISSIONS = "100"
    ENVIRONMENT_MAX_CAUSAL_DEPTH           = "8"
    ENVIRONMENT_ADMISSION_RATE_PER_SECOND  = "10"
    ENVIRONMENT_ADMISSION_BURST            = "20"
    ENVIRONMENT_PREPARATION_TIMEOUT_MS     = "600000"
    COMPUTER_KMS_KEY_ARN                   = "other"
  } }
  expect_failures = [terraform_data.bootstrap_preconditions]
}

run "dispatcher_observes_cloudfront_viewer_endpoint" {
  command = apply

  variables {
    enable_cloudfront             = true
    cloudfront_origin_domain_name = "origin.example.test"
    certificate_arn               = "arn:aws:acm:us-east-1:000000000000:certificate/00000000-0000-0000-0000-000000000000"
  }

  override_resource {
    target = aws_cloudfront_distribution.controlplane
    values = { domain_name = "viewer.cloudfront.net" }
  }

  assert {
    condition     = { for item in jsondecode(aws_ecs_task_definition.dispatcher.container_definitions)[0].environment : item.name => item.value }.CONTROL_PLANE_URL == "https://viewer.cloudfront.net"
    error_message = "Dispatcher must probe the Worker-facing viewer URL, not the restricted ALB origin."
  }
}

run "environment_execution_policy_is_required" {
  command = plan
  variables { controlplane_environment = {
    DEPLOYMENT_UPLOAD_PROCESS_CONCURRENCY      = "8"
    DEPLOYMENT_UPLOAD_ORGANIZATION_CONCURRENCY = "8"
    DEPLOYMENT_UPLOAD_IDLE_TIMEOUT             = "2m"
    DEPLOYMENT_UPLOAD_MAX_DURATION             = "2h"
  } }
  expect_failures = [var.controlplane_environment]
}
run "environment_execution_policy_reaches_container" {
  command = plan
  assert {
    condition     = alltrue([for name, value in var.controlplane_environment : { for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].environment : item.name => item.value }[name] == value])
    error_message = "Configured execution policy must reach the control-plane process unchanged."
  }
}

run "slack_credentials_are_database_owned" {
  command = plan
  assert {
    condition = alltrue([for container in concat(
      jsondecode(aws_ecs_task_definition.controlplane.container_definitions),
      jsondecode(aws_ecs_task_definition.dispatcher.container_definitions)
    ) : alltrue([for item in concat(container.environment, container.secrets) : !startswith(item.name, "SLACK_") || item.name == "SLACK_TRANSPORT"])])
    error_message = "Dedicated Slack app credentials belong to protected registrations, not process configuration."
  }
}

run "private_ingress" {
  command = apply
  variables {
    allow_insecure_http       = false
    ingress_mode              = "private"
    app_origin                = "https://helmr.internal"
    certificate_arn           = "arn:aws:acm:us-east-1:000000000000:certificate/00000000-0000-0000-0000-000000000000"
    private_client_ipv4_cidrs = ["10.20.0.0/16"]
    restrict_platform_egress  = true
    slack_transport           = "socket"
    outbound_proxy_url        = "http://proxy.internal:3128"
  }
  assert {
    condition     = aws_lb.controlplane.internal && toset(aws_lb.controlplane.subnets) == toset(var.private_subnet_ids) && length(aws_lb_listener.http) == 0 && length(aws_lb_listener.http_redirect) == 0 && length(aws_cloudfront_distribution.controlplane) == 0
    error_message = "Private ingress must have only an internal HTTPS listener."
  }
  assert {
    condition     = length(aws_vpc_security_group_ingress_rule.alb_http_public) == 0 && length(aws_vpc_security_group_ingress_rule.alb_https) == 0 && length(aws_vpc_security_group_egress_rule.controlplane) == 0 && aws_vpc_security_group_egress_rule.alb.to_port == 8080
    error_message = "Private ingress or restricted egress widened unexpectedly."
  }
  assert {
    condition     = alltrue([for task in [aws_ecs_task_definition.controlplane, aws_ecs_task_definition.dispatcher] : { for item in jsondecode(task.container_definitions)[0].environment : item.name => item.value }.SLACK_TRANSPORT == "socket" && { for item in jsondecode(task.container_definitions)[0].environment : item.name => item.value }.OUTBOUND_PROXY_URL == "http://proxy.internal:3128"])
    error_message = "API and Dispatcher must receive the same operator-selected integration path."
  }
}

run "private_rejects_public_task_addresses" {
  command = plan
  variables {
    ingress_mode                  = "private"
    private_client_ipv4_cidrs     = ["10.20.0.0/16"]
    controlplane_assign_public_ip = true
  }
  expect_failures = [terraform_data.ingress_contract]
}

run "private_rejects_implicit_sources" {
  command = plan
  variables { ingress_mode = "private" }
  expect_failures = [terraform_data.ingress_contract]
}

run "audit_purge_is_bounded_and_isolated" {
  command = apply
  variables { create_controlplane_service = true }
  assert {
    condition     = jsondecode(aws_ecs_task_definition.audit_maintenance.container_definitions)[0].command == ["audit", "purge"]
    error_message = "Periodic maintenance must only run purge."
  }
  assert {
    condition     = [for s in jsondecode(aws_ecs_task_definition.audit_maintenance.container_definitions)[0].secrets : s.name] == ["DATABASE_AUDIT_MAINTENANCE_URL"]
    error_message = "Purge must not receive migration, runtime or unrelated secrets."
  }
  assert {
    condition     = alltrue([for task in [aws_ecs_task_definition.controlplane, aws_ecs_task_definition.dispatcher] : !contains([for s in jsondecode(task.container_definitions)[0].secrets : s.name], "DATABASE_AUDIT_MAINTENANCE_URL")])
    error_message = "Serving tasks must not receive audit deletion authority."
  }
  assert {
    condition     = { for e in jsondecode(aws_ecs_task_definition.audit_maintenance.container_definitions)[0].environment : e.name => e.value }.AUDIT_PURGE_MAX_BATCHES == "50" && aws_cloudwatch_event_rule.audit_purge.schedule_expression == "rate(15 minutes)"
    error_message = "Purge must have finite execution and an installed periodic schedule."
  }
  assert {
    condition     = aws_cloudwatch_metric_alarm.audit_purge_missing[0].treat_missing_data == "breaching"
    error_message = "Missing or failed maintenance must be observable without a successful task launch."
  }
}

run "audit_purge_rejects_deadline_without_setup_margin" {
  command = plan
  variables {
    audit_maintenance_timeout_seconds = 300
    audit_purge_max_batches           = 60
  }
  expect_failures = [aws_ecs_task_definition.audit_maintenance]
}

run "live_controlplane_requires_diagnostic_limits" {
  command = plan
  variables {
    create_controlplane_service = true
    dispatcher_desired_count    = 0
    controlplane_environment = {
      DEPLOYMENT_UPLOAD_PROCESS_CONCURRENCY      = "8"
      DEPLOYMENT_UPLOAD_ORGANIZATION_CONCURRENCY = "8"
      DEPLOYMENT_UPLOAD_IDLE_TIMEOUT             = "2m"
      DEPLOYMENT_UPLOAD_MAX_DURATION             = "2h"

      ENVIRONMENT_MAX_RESIDENT_COMPUTERS     = "10"
      ENVIRONMENT_MAX_CPU_MILLIS             = "16000"
      ENVIRONMENT_MAX_MEMORY_BYTES           = "68719476736"
      ENVIRONMENT_MAX_RESERVED_STORAGE_BYTES = "1099511627776"
      ENVIRONMENT_MAX_OUTSTANDING_ADMISSIONS = "100"
      ENVIRONMENT_MAX_CAUSAL_DEPTH           = "8"
      ENVIRONMENT_ADMISSION_RATE_PER_SECOND  = "10"
      ENVIRONMENT_ADMISSION_BURST            = "20"
      ENVIRONMENT_PREPARATION_TIMEOUT_MS     = "600000"
    }
  }
  expect_failures = [var.controlplane_environment]
}
run "live_dispatcher_requires_diagnostic_limits" {
  command = plan
  variables {
    create_controlplane_service = true
    controlplane_desired_count  = 0
    dispatcher_environment      = {}
  }
  expect_failures = [var.dispatcher_environment]
}
run "zero_count_services_allow_missing_diagnostic_limits" {
  command = plan
  variables {
    create_controlplane_service = true
    controlplane_desired_count  = 0
    dispatcher_desired_count    = 0
    dispatcher_environment      = {}
    controlplane_environment = {
      DEPLOYMENT_UPLOAD_PROCESS_CONCURRENCY      = "8"
      DEPLOYMENT_UPLOAD_ORGANIZATION_CONCURRENCY = "8"
      DEPLOYMENT_UPLOAD_IDLE_TIMEOUT             = "2m"
      DEPLOYMENT_UPLOAD_MAX_DURATION             = "2h"

      ENVIRONMENT_MAX_RESIDENT_COMPUTERS     = "10"
      ENVIRONMENT_MAX_CPU_MILLIS             = "16000"
      ENVIRONMENT_MAX_MEMORY_BYTES           = "68719476736"
      ENVIRONMENT_MAX_RESERVED_STORAGE_BYTES = "1099511627776"
      ENVIRONMENT_MAX_OUTSTANDING_ADMISSIONS = "100"
      ENVIRONMENT_MAX_CAUSAL_DEPTH           = "8"
      ENVIRONMENT_ADMISSION_RATE_PER_SECOND  = "10"
      ENVIRONMENT_ADMISSION_BURST            = "20"
      ENVIRONMENT_PREPARATION_TIMEOUT_MS     = "600000"
    }
  }
}
run "bootstrap_only_allows_missing_diagnostic_limits" {
  command = plan
  variables {
    create_controlplane_service = false
    dispatcher_environment      = {}
    controlplane_environment = {
      DEPLOYMENT_UPLOAD_PROCESS_CONCURRENCY      = "8"
      DEPLOYMENT_UPLOAD_ORGANIZATION_CONCURRENCY = "8"
      DEPLOYMENT_UPLOAD_IDLE_TIMEOUT             = "2m"
      DEPLOYMENT_UPLOAD_MAX_DURATION             = "2h"

      ENVIRONMENT_MAX_RESIDENT_COMPUTERS     = "10"
      ENVIRONMENT_MAX_CPU_MILLIS             = "16000"
      ENVIRONMENT_MAX_MEMORY_BYTES           = "68719476736"
      ENVIRONMENT_MAX_RESERVED_STORAGE_BYTES = "1099511627776"
      ENVIRONMENT_MAX_OUTSTANDING_ADMISSIONS = "100"
      ENVIRONMENT_MAX_CAUSAL_DEPTH           = "8"
      ENVIRONMENT_ADMISSION_RATE_PER_SECOND  = "10"
      ENVIRONMENT_ADMISSION_BURST            = "20"
      ENVIRONMENT_PREPARATION_TIMEOUT_MS     = "600000"
    }
  }
}
run "blocked_prefixes_require_numeric_order" {
  command = plan
  variables { network_blocked_ipv4_cidrs = ["10.0.0.0/8", "2.0.0.0/8"] }
  expect_failures = [var.network_blocked_ipv4_cidrs]
}
run "equal_address_prefixes_require_length_order" {
  command = plan
  variables { network_blocked_ipv4_cidrs = ["10.0.0.0/16", "10.0.0.0/8"] }
  expect_failures = [var.network_blocked_ipv4_cidrs]
}
run "blocked_prefixes_accept_numeric_not_lexical_order" {
  command = plan
  variables { network_blocked_ipv4_cidrs = ["2.0.0.0/8", "10.0.0.0/8", "10.0.0.0/16"] }
}
run "private_business_prefixes_require_numeric_order" {
  command = plan
  variables { network_private_business_cidrs = ["192.168.0.0/16", "10.0.0.0/8"] }
  expect_failures = [var.network_private_business_cidrs]
}

run "live_controlplane_diagnostic_limits_require_positive_integers" {
  command = plan
  variables {
    create_controlplane_service = true
    dispatcher_desired_count    = 0
    controlplane_environment = {
      DEPLOYMENT_UPLOAD_PROCESS_CONCURRENCY      = "8"
      DEPLOYMENT_UPLOAD_ORGANIZATION_CONCURRENCY = "8"
      DEPLOYMENT_UPLOAD_IDLE_TIMEOUT             = "2m"
      DEPLOYMENT_UPLOAD_MAX_DURATION             = "2h"

      DIAGNOSTIC_CHUNK_BYTES                 = "0"
      DIAGNOSTIC_SOURCE_BYTES                = "4194304"
      DIAGNOSTIC_SOURCE_RECORDS              = "1024"
      DIAGNOSTIC_ENVIRONMENT_BYTES           = "16777216"
      DIAGNOSTIC_ENVIRONMENT_RECORDS         = "4096"
      DIAGNOSTIC_QUEUE_BYTES                 = "67108864"
      DIAGNOSTIC_QUEUE_RECORDS               = "16384"
      DIAGNOSTIC_DB_MAX_CONNECTIONS          = "2"
      ENVIRONMENT_MAX_RESIDENT_COMPUTERS     = "10"
      ENVIRONMENT_MAX_CPU_MILLIS             = "16000"
      ENVIRONMENT_MAX_MEMORY_BYTES           = "68719476736"
      ENVIRONMENT_MAX_RESERVED_STORAGE_BYTES = "1099511627776"
      ENVIRONMENT_MAX_OUTSTANDING_ADMISSIONS = "100"
      ENVIRONMENT_MAX_CAUSAL_DEPTH           = "8"
      ENVIRONMENT_ADMISSION_RATE_PER_SECOND  = "10"
      ENVIRONMENT_ADMISSION_BURST            = "20"
      ENVIRONMENT_PREPARATION_TIMEOUT_MS     = "600000"
    }
  }
  expect_failures = [var.controlplane_environment]
}

run "live_dispatcher_diagnostic_limits_require_positive_integers" {
  command = plan
  variables {
    create_controlplane_service = true
    controlplane_desired_count  = 0
    dispatcher_environment = {
      DIAGNOSTIC_EXPORT_BATCH_BYTES   = "4194304"
      DIAGNOSTIC_EXPORT_BATCH_RECORDS = "1.5"
      DIAGNOSTIC_CHUNK_BYTES          = "65536"
      DIAGNOSTIC_SOURCE_BYTES         = "4194304"
      DIAGNOSTIC_SOURCE_RECORDS       = "1024"
      DIAGNOSTIC_ENVIRONMENT_BYTES    = "16777216"
      DIAGNOSTIC_ENVIRONMENT_RECORDS  = "4096"
      DIAGNOSTIC_QUEUE_BYTES          = "67108864"
      DIAGNOSTIC_QUEUE_RECORDS        = "16384"
      DIAGNOSTIC_DB_MAX_CONNECTIONS   = "2"
    }
  }
  expect_failures = [var.dispatcher_environment]
}

run "quarantine_retention_is_always_bounded" {
  command = apply
  assert {
    condition     = length(aws_s3_bucket_lifecycle_configuration.cas) == 1 && length(aws_s3_bucket_lifecycle_configuration.cas[0].rule) == 2
    error_message = "Quarantine retention must be present without enabling expiration of published CAS objects."
  }
  assert {
    condition     = alltrue([for rule in aws_s3_bucket_lifecycle_configuration.cas[0].rule : rule.filter[0].prefix == "quarantine/" && rule.status == "Enabled"])
    error_message = "Mandatory retention must cover only the canonical quarantine prefix."
  }
  assert {
    condition     = one([for rule in aws_s3_bucket_lifecycle_configuration.cas[0].rule : rule if rule.id == "expire-quarantine"]).expiration[0].days == 1 && one([for rule in aws_s3_bucket_lifecycle_configuration.cas[0].rule : rule if rule.id == "expire-quarantine"]).noncurrent_version_expiration[0].noncurrent_days == 1 && one([for rule in aws_s3_bucket_lifecycle_configuration.cas[0].rule : rule if rule.id == "remove-quarantine-delete-markers"]).expiration[0].expired_object_delete_marker
    error_message = "Quarantine current versions, noncurrent versions and delete markers must all expire."
  }
  assert {
    condition     = length(aws_s3_bucket_policy.quarantine) == 0
    error_message = "Public installations must not require private storage connectivity."
  }
}

run "private_quarantine_rejects_outside_endpoint" {
  command = apply
  variables {
    ingress_mode              = "private"
    private_client_ipv4_cidrs = ["10.20.0.0/16"]
    allow_insecure_http       = false
    app_origin                = "https://helmr.internal"
    certificate_arn           = "arn:aws:acm:us-east-1:000000000000:certificate/00000000-0000-0000-0000-000000000000"
  }
  assert {
    condition     = jsondecode(aws_s3_bucket_policy.quarantine[0].policy).Statement == [{ Sid = "RequirePrivateQuarantineUpload", Effect = "Deny", Principal = "*", Action = "s3:PutObject", Resource = "${aws_s3_bucket.cas.arn}/quarantine/*", Condition = { StringNotEquals = { "aws:SourceVpce" = var.quarantine_upload_vpc_endpoint_id } } }]
    error_message = "Private quarantine writes must require the Control Plane endpoint without principal bypasses or a whole-CAS deny."
  }
}

run "private_quarantine_requires_endpoint" {
  command = plan
  variables {
    ingress_mode                      = "private"
    private_client_ipv4_cidrs         = ["10.20.0.0/16"]
    quarantine_upload_vpc_endpoint_id = null
    allow_insecure_http               = false
    app_origin                        = "https://helmr.internal"
    certificate_arn                   = "arn:aws:acm:us-east-1:000000000000:certificate/00000000-0000-0000-0000-000000000000"
  }
  expect_failures = [aws_s3_bucket_policy.quarantine]
}
