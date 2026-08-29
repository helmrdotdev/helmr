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
  name                              = "helmr-test"
  vpc_id                            = "vpc-0123456789abcdef0"
  private_subnet_ids                = ["subnet-0123456789abcdef0", "subnet-1123456789abcdef0"]
  public_subnet_ids                 = ["subnet-2123456789abcdef0", "subnet-3123456789abcdef0"]
  public_url                        = "http://controlplane.example.test"
  allow_insecure_http               = true
  bootstrap_worker_group_name       = "default"
  bootstrap_region_id               = "helmr-us-east"
  controlplane_image                = "example.invalid/helmr@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  controlplane_image_repository_arn = "arn:aws:ecr:us-east-1:000000000000:repository/helmr-test/controlplane-releases"
  platform_store_uri                = "s3://helmr-test-runtime/objects"
  platform_store_bucket_arn         = "arn:aws:s3:::helmr-test-runtime"
  platform_store_kms_key_arn        = "arn:aws:kms:us-east-1:000000000000:key/11111111-1111-1111-1111-111111111111"
  clickhouse_url                    = "https://clickhouse.example.invalid"
  github_oauth_client_id            = "test-client"
  database_skip_final_snapshot      = true
}

run "controlplane_uses_execution_only_runtime_authority" {
  command = apply

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
      toset([for item in jsondecode(aws_ecs_task_definition.migration.container_definitions)[0].environment : item.name]) == toset(["CLICKHOUSE_URL"])
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
