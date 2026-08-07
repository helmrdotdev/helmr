mock_provider "aws" {
  mock_data "aws_region" {
    defaults = {
      region = "us-east-1"
    }
  }

  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "000000000000"
    }
  }

  mock_data "aws_partition" {
    defaults = {
      partition  = "aws"
      dns_suffix = "amazonaws.com"
    }
  }

  mock_resource "aws_secretsmanager_secret" {
    defaults = {
      arn = "arn:aws:secretsmanager:us-east-1:000000000000:secret:mock"
    }
  }

  mock_resource "aws_s3_bucket" {
    defaults = {
      arn = "arn:aws:s3:::mock-bucket"
    }
  }

  mock_resource "aws_kms_key" {
    defaults = {
      arn = "arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000000"
    }
  }

  mock_resource "aws_iam_role" {
    defaults = {
      arn = "arn:aws:iam::000000000000:role/mock"
    }
  }

  mock_resource "aws_iam_policy" {
    defaults = {
      arn = "arn:aws:iam::000000000000:policy/mock"
    }
  }

  mock_resource "aws_elasticache_replication_group" {
    defaults = {
      primary_endpoint_address = "redis.example.test"
    }
  }
}

mock_provider "random" {}

override_resource {
  target = aws_kms_key.helmr
  values = {
    arn = "arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000000"
  }
}

override_resource {
  target = aws_lb.controlplane
  values = {
    arn = "arn:aws:elasticloadbalancing:us-east-1:000000000000:loadbalancer/app/helmr-test/0000000000000000"
  }
}

override_resource {
  target = aws_lb_target_group.controlplane
  values = {
    arn = "arn:aws:elasticloadbalancing:us-east-1:000000000000:targetgroup/helmr-test/0000000000000000"
  }
}

override_resource {
  target = aws_iam_role.controlplane_execution
  values = {
    arn = "arn:aws:iam::000000000000:role/helmr-test-controlplane-execution"
  }
}

override_resource {
  target = aws_iam_role.dispatcher_execution
  values = {
    arn = "arn:aws:iam::000000000000:role/helmr-test-dispatcher-execution"
  }
}

override_resource {
  target = aws_iam_role.database_bootstrap_execution
  values = {
    arn = "arn:aws:iam::000000000000:role/helmr-test-database-bootstrap-execution"
  }
}

override_resource {
  target = aws_iam_role.controlplane_task
  values = {
    arn = "arn:aws:iam::000000000000:role/helmr-test-controlplane-task"
  }
}

override_resource {
  target = aws_iam_role.image_cache
  values = {
    arn = "arn:aws:iam::000000000000:role/helmr-test-image-cache"
  }
}

override_resource {
  target = aws_iam_role.dispatcher_task
  values = {
    arn = "arn:aws:iam::000000000000:role/helmr-test-dispatcher-task"
  }
}

override_resource {
  target = aws_iam_role.migration_task
  values = {
    arn = "arn:aws:iam::000000000000:role/helmr-test-migration-task"
  }
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
  image_cache_worker_role_arns      = ["arn:aws:iam::000000000000:role/helmr-run"]
  bootstrap_region_id               = "helmr-us-east"
  controlplane_image                = "example.invalid/helmr@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  controlplane_image_repository_arn = "arn:aws:ecr:us-east-1:000000000000:repository/helmr-test/controlplane-releases"
  platform_store_uri                = "s3://helmr-test-runtime/objects"
  platform_store_bucket_arn         = "arn:aws:s3:::helmr-test-runtime"
  platform_store_kms_key_arn        = "arn:aws:kms:us-east-1:000000000000:key/11111111-1111-1111-1111-111111111111"
  build_policy_digest               = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  clickhouse_url                    = "https://clickhouse.example.invalid"
  github_oauth_client_id            = "test-client"
  database_skip_final_snapshot      = true
}

run "controlplane_installs_exact_policy_before_start" {
  command = apply

  assert {
    condition = (
      jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].name == "release-install" &&
      jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].user == "0" &&
      jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].entryPoint == ["helmr-controlplane"] &&
      jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[0].command == [
        "release",
        "install",
        "--store",
        var.platform_store_uri,
        "--digest",
        var.build_policy_digest,
        "--output",
        "/release/build-policy.json"
      ]
    )
    error_message = "Control Plane must use the exact digest-pinned release install command in a root init container."
  }

  assert {
    condition = (
      { for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[1].environment : item.name => item.value }.PUBLIC_URL == "http://controlplane.example.test" &&
      { for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[1].environment : item.name => item.value }.API_ORIGIN == "http://controlplane.example.test" &&
      aws_lb_target_group.controlplane.health_check[0].path == "/readyz" &&
      contains([for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[1].secrets : item.name], "SETUP_TOKEN")
    )
    error_message = "Self-hosted Control Plane must default API_ORIGIN to PUBLIC_URL, use readiness for traffic, and receive the setup token."
  }

  assert {
    condition = (
      jsondecode(aws_ecs_task_definition.database_bootstrap.container_definitions)[0].command == ["database-bootstrap"] &&
      toset([for item in jsondecode(aws_ecs_task_definition.database_bootstrap.container_definitions)[0].environment : item.name]) == toset(["DATABASE_ADMIN_HOST", "DATABASE_ADMIN_PORT", "DATABASE_NAME"]) &&
      toset([for item in jsondecode(aws_ecs_task_definition.database_bootstrap.container_definitions)[0].secrets : item.name]) == toset(["DATABASE_ADMIN_USERNAME", "DATABASE_ADMIN_PASSWORD", "DATABASE_URL"]) &&
      strcontains(aws_iam_role_policy.database_bootstrap_execution.policy, "database-master") &&
      !strcontains(aws_iam_role_policy.controlplane_execution.policy, "database-master") &&
      !strcontains(aws_iam_role_policy.dispatcher_execution.policy, "database-master")
    )
    error_message = "Only the one-shot database bootstrap task may receive the RDS master credential."
  }

  assert {
    condition = (
      jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[1].dependsOn == [{
        containerName = "release-install"
        condition     = "SUCCESS"
      }] &&
      jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[1].mountPoints == [{
        sourceVolume  = "release-policy"
        containerPath = "/release"
        readOnly      = true
      }]
    )
    error_message = "Control Plane must depend on successful installation and mount the build policy volume read-only."
  }

  assert {
    condition = (
      { for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[1].environment : item.name => item.value }.BUILD_POLICY_PATH == "/release/build-policy.json" &&
      { for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[1].environment : item.name => item.value }.PLATFORM_STORE_URI == var.platform_store_uri
    )
    error_message = "Only the main Control Plane container must load the installed policy and immutable Platform Artifact store."
  }

  assert {
    condition = (
      { for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[1].environment : item.name => item.value }.BOOTSTRAP_ENABLED == "1" &&
      { for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[1].environment : item.name => item.value }.BOOTSTRAP_REGION_ID == "helmr-us-east" &&
      { for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[1].environment : item.name => item.value }.BOOTSTRAP_WORKER_GROUP_NAME == "default" &&
      contains(
        [for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[1].secrets : item.name],
        "BOOTSTRAP_WORKER_TOKEN"
      )
    )
    error_message = "Control Plane must receive one explicit Region and Worker Group bootstrap plus its token secret."
  }

  assert {
    condition = (
      !strcontains(aws_iam_role_policy.controlplane_execution.policy, "ec2:DescribeInstances") &&
      !strcontains(aws_iam_role_policy.controlplane_execution.policy, "autoscaling:DescribeAutoScalingInstances") &&
      !strcontains(aws_iam_role_policy.controlplane_execution.policy, "iam:GetInstanceProfile") &&
      !strcontains(aws_iam_role_policy.controlplane_task.policy, "ec2:DescribeInstances") &&
      !strcontains(aws_iam_role_policy.controlplane_task.policy, "autoscaling:DescribeAutoScalingInstances") &&
      !strcontains(aws_iam_role_policy.controlplane_task.policy, "iam:GetInstanceProfile")
    )
    error_message = "Control Plane enrollment must not regain AWS inventory authority."
  }

  assert {
    condition = (
      !contains([for item in jsondecode(aws_ecs_task_definition.dispatcher.container_definitions)[0].environment : item.name], "BUILD_POLICY_PATH") &&
      !contains([for item in jsondecode(aws_ecs_task_definition.dispatcher.container_definitions)[0].environment : item.name], "PLATFORM_STORE_URI") &&
      !contains([for item in jsondecode(aws_ecs_task_definition.migration.container_definitions)[0].environment : item.name], "BUILD_POLICY_PATH") &&
      !contains([for item in jsondecode(aws_ecs_task_definition.migration.container_definitions)[0].environment : item.name], "PLATFORM_STORE_URI")
    )
    error_message = "Dispatcher and migration must not receive build policy configuration."
  }

  assert {
    condition = (
      toset([for item in jsondecode(aws_ecs_task_definition.migration.container_definitions)[0].environment : item.name]) == toset(["CLICKHOUSE_URL"]) &&
      toset([for item in jsondecode(aws_ecs_task_definition.migration.container_definitions)[0].secrets : item.name]) == toset(["DATABASE_URL"])
    )
    error_message = "Migration must receive only PostgreSQL and ClickHouse migration configuration, without Control Plane region or Worker configuration."
  }

  assert {
    condition = (
      strcontains(aws_iam_role_policy.controlplane_task.policy, "${var.platform_store_bucket_arn}/objects/sha256/*") &&
      !strcontains(aws_iam_role_policy.controlplane_task.policy, "PublishRetainedArtifacts") &&
      !strcontains(aws_iam_role_policy.controlplane_task.policy, "ReadRetainedArtifacts") &&
      !strcontains(aws_iam_role_policy.controlplane_task.policy, "${var.platform_store_bucket_arn}/controlplane/runtime") &&
      aws_ecs_task_definition.migration.task_role_arn == aws_iam_role.migration_task.arn &&
      aws_iam_role.migration_task.name == "helmr-test-migration-task"
    )
    error_message = "Control Plane IAM must read only runtime objects, exclude Manager storage, rollout lineage, and retained artifacts, and not leak to migration."
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
      { for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[1].environment : item.name => item.value }.API_ORIGIN == "https://api.example.test" &&
      !contains([for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[1].secrets : item.name], "SETUP_TOKEN") &&
      !contains(keys(output.secret_arns), "setup_token")
    )
    error_message = "Managed Control Plane must use the explicit API origin without creating or injecting a setup token."
  }
}

run "execution_roles_are_pull_only_for_the_exact_controlplane_repository" {
  command = plan

  assert {
    condition = (
      strcontains(aws_iam_role_policy.controlplane_execution.policy, var.controlplane_image_repository_arn) &&
      strcontains(aws_iam_role_policy.dispatcher_execution.policy, var.controlplane_image_repository_arn) &&
      strcontains(aws_iam_role_policy.controlplane_execution.policy, "ecr:BatchGetImage") &&
      strcontains(aws_iam_role_policy.dispatcher_execution.policy, "ecr:GetDownloadUrlForLayer") &&
      !strcontains(aws_iam_role_policy.controlplane_execution.policy, "ecr:PutImage") &&
      !strcontains(aws_iam_role_policy.dispatcher_execution.policy, "ecr:PutImage") &&
      !strcontains(aws_iam_role_policy.controlplane_execution.policy, "ecr:Delete") &&
      !strcontains(aws_iam_role_policy.dispatcher_execution.policy, "ecr:Delete")
    )
    error_message = "ECS execution roles must have pull-only access to the exact Control Plane release repository."
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
      contains(
        [for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[1].secrets : item.name],
        "CAPACITY_TOKEN"
      ) &&
      !contains(
        [for item in jsondecode(aws_ecs_task_definition.dispatcher.container_definitions)[0].secrets : item.name],
        "CAPACITY_TOKEN"
      )
    )
    error_message = "only Control Plane receives the externally composed capacity credential"
  }
}

run "capacity_api_credential_rejects_plaintext_environment" {
  command = plan

  variables {
    controlplane_environment = {
      CAPACITY_TOKEN = "plaintext-must-not-enter-task-definition"
    }
  }

  expect_failures = [terraform_data.bootstrap_preconditions]
}

run "workspace_image_cache_is_exact_and_controlplane_only" {
  command = plan

  assert {
    condition = (
      { for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[1].environment : item.name => item.value }.IMAGE_CACHE_REGISTRY_AUTHORITY == "000000000000.dkr.ecr.us-east-1.amazonaws.com" &&
      { for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[1].environment : item.name => item.value }.IMAGE_CACHE_REPOSITORY_PREFIX == "helmr-test/image-cache" &&
      { for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[1].environment : item.name => item.value }.IMAGE_CACHE_ROLE_ARN == "arn:aws:iam::000000000000:role/helmr-test-image-cache" &&
      { for item in jsondecode(aws_ecs_task_definition.controlplane.container_definitions)[1].environment : item.name => item.value }.IMAGE_CACHE_REPOSITORY_ARN_PREFIX == "arn:aws:ecr:us-east-1:000000000000:repository/helmr-test/image-cache/" &&
      !contains([for item in jsondecode(aws_ecs_task_definition.dispatcher.container_definitions)[0].environment : item.name], "IMAGE_CACHE_ROLE_ARN") &&
      !contains([for item in jsondecode(aws_ecs_task_definition.migration.container_definitions)[0].environment : item.name], "IMAGE_CACHE_ROLE_ARN")
    )
    error_message = "only Control Plane must receive the complete exact Execution image-cache configuration"
  }

  assert {
    condition = (
      strcontains(aws_iam_role_policy.controlplane_task.policy, "ProvisionExecutionImageCache") &&
      strcontains(aws_iam_role_policy.controlplane_task.policy, "arn:aws:ecr:us-east-1:000000000000:repository/helmr-test/image-cache/environments/*") &&
      strcontains(aws_iam_role_policy.controlplane_task.policy, "ecr:CreateRepository") &&
      strcontains(aws_iam_role_policy.controlplane_task.policy, "ecr:SetRepositoryPolicy") &&
      length(jsondecode(aws_iam_role.image_cache.assume_role_policy).Statement) == 1 &&
      jsondecode(aws_iam_role.image_cache.assume_role_policy).Statement[0].Effect == "Allow" &&
      jsondecode(aws_iam_role.image_cache.assume_role_policy).Statement[0].Action == "sts:AssumeRole" &&
      keys(jsondecode(aws_iam_role.image_cache.assume_role_policy).Statement[0].Principal) == ["AWS"] &&
      jsondecode(aws_iam_role.image_cache.assume_role_policy).Statement[0].Principal.AWS == "arn:aws:iam::000000000000:root" &&
      keys(jsondecode(aws_iam_role.image_cache.assume_role_policy).Statement[0].Condition) == ["ArnEquals"] &&
      keys(jsondecode(aws_iam_role.image_cache.assume_role_policy).Statement[0].Condition.ArnEquals) == ["aws:PrincipalArn"] &&
      jsondecode(aws_iam_role.image_cache.assume_role_policy).Statement[0].Condition.ArnEquals["aws:PrincipalArn"] == ["arn:aws:iam::000000000000:role/helmr-run"] &&
      !strcontains(aws_iam_role.image_cache.assume_role_policy, "*") &&
      !strcontains(aws_iam_role.image_cache.assume_role_policy, "ArnLike") &&
      !strcontains(aws_iam_role.image_cache.assume_role_policy, ":sts::") &&
      !strcontains(aws_iam_role.image_cache.assume_role_policy, "instance-profile") &&
      strcontains(aws_iam_policy.image_cache_boundary.policy, "ecr:GetAuthorizationToken") &&
      strcontains(aws_iam_policy.image_cache_boundary.policy, "ecr:UploadLayerPart")
    )
    error_message = "Control Plane provisioning and Worker cache-role authority must be bounded to the Environment repository namespace"
  }
}
