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
  target = aws_lb.control
  values = {
    arn = "arn:aws:elasticloadbalancing:us-east-1:000000000000:loadbalancer/app/helmr-test/0000000000000000"
  }
}

override_resource {
  target = aws_lb_target_group.control
  values = {
    arn = "arn:aws:elasticloadbalancing:us-east-1:000000000000:targetgroup/helmr-test/0000000000000000"
  }
}

override_resource {
  target = aws_iam_role.control_execution
  values = {
    arn = "arn:aws:iam::000000000000:role/helmr-test-control-execution"
  }
}

override_resource {
  target = aws_iam_role.dispatcher_execution
  values = {
    arn = "arn:aws:iam::000000000000:role/helmr-test-dispatcher-execution"
  }
}

override_resource {
  target = aws_iam_role.control_task
  values = {
    arn = "arn:aws:iam::000000000000:role/helmr-test-control-task"
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
    master_user_secret = [{
      kms_key_id    = "arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000000"
      secret_arn    = "arn:aws:secretsmanager:us-east-1:000000000000:secret:database-master"
      secret_status = "active"
    }]
  }
}

variables {
  name                = "helmr-test"
  vpc_id              = "vpc-0123456789abcdef0"
  private_subnet_ids  = ["subnet-0123456789abcdef0", "subnet-1123456789abcdef0"]
  public_subnet_ids   = ["subnet-2123456789abcdef0", "subnet-3123456789abcdef0"]
  public_url          = "http://control.example.test"
  allow_insecure_http = true
  worker_group_id     = "run-workers"
  worker_groups = [{
    id                      = "run-workers"
    name                    = "Run workers"
    region                  = "helmr-us-east"
    account_id              = "000000000000"
    autoscaling_group       = "helmr-run"
    instance_profile_arn    = "arn:aws:iam::000000000000:instance-profile/helmr-run"
    instance_role_arn       = "arn:aws:iam::000000000000:role/helmr-run"
    launch_ami_id           = "ami-0123456789abcdef0"
    ami_ids                 = ["ami-0123456789abcdef0"]
    allows_run              = true
    allows_build            = true
    observation_ttl_seconds = 120
    instance_capacity = {
      milli_cpu                  = 4000
      memory_bytes               = 8589934592
      guest_ephemeral_disk_bytes = 34359738368
      build_cache_bytes          = 0
      artifact_cache_bytes       = 0
      vm_slots                   = 2
      build_executors            = 1
    }
  }]
  region_id                    = "helmr-us-east"
  default_region_id            = "helmr-us-east"
  control_image                = "example.invalid/helmr@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  control_image_repository_arn = "arn:aws:ecr:us-east-1:000000000000:repository/helmr-test/control-releases"
  platform_store_uri           = "s3://helmr-test-runtime/objects"
  platform_store_bucket_arn    = "arn:aws:s3:::helmr-test-runtime"
  platform_store_kms_key_arn   = "arn:aws:kms:us-east-1:000000000000:key/11111111-1111-1111-1111-111111111111"
  build_policy_digest          = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  clickhouse_url               = "https://clickhouse.example.invalid"
  github_oauth_client_id       = "test-client"
  database_skip_final_snapshot = true
}

run "control_installs_exact_policy_before_start" {
  command = apply

  assert {
    condition = (
      jsondecode(aws_ecs_task_definition.control.container_definitions)[0].name == "release-install" &&
      jsondecode(aws_ecs_task_definition.control.container_definitions)[0].user == "0" &&
      jsondecode(aws_ecs_task_definition.control.container_definitions)[0].entryPoint == ["helmr-control"] &&
      jsondecode(aws_ecs_task_definition.control.container_definitions)[0].command == [
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
    error_message = "Control must use the exact digest-pinned release install command in a root init container."
  }

  assert {
    condition = (
      jsondecode(aws_ecs_task_definition.control.container_definitions)[1].dependsOn == [{
        containerName = "release-install"
        condition     = "SUCCESS"
      }] &&
      jsondecode(aws_ecs_task_definition.control.container_definitions)[1].mountPoints == [{
        sourceVolume  = "release-policy"
        containerPath = "/release"
        readOnly      = true
      }]
    )
    error_message = "Control must depend on successful installation and mount the build policy volume read-only."
  }

  assert {
    condition = (
      { for item in jsondecode(aws_ecs_task_definition.control.container_definitions)[1].environment : item.name => item.value }.HELMR_BUILD_POLICY_PATH == "/release/build-policy.json" &&
      { for item in jsondecode(aws_ecs_task_definition.control.container_definitions)[1].environment : item.name => item.value }.HELMR_PLATFORM_STORE_URI == var.platform_store_uri &&
      !contains([for item in jsondecode(aws_ecs_task_definition.control.container_definitions)[1].environment : item.name], "HELMR_RETAINED_CAS_URI")
    )
    error_message = "Only the main Control container must load the installed policy and immutable Platform Artifact store."
  }

  assert {
    condition = (
      !contains([for item in jsondecode(aws_ecs_task_definition.dispatcher.container_definitions)[0].environment : item.name], "HELMR_BUILD_POLICY_PATH") &&
      !contains([for item in jsondecode(aws_ecs_task_definition.dispatcher.container_definitions)[0].environment : item.name], "HELMR_PLATFORM_STORE_URI") &&
      !contains([for item in jsondecode(aws_ecs_task_definition.migration.container_definitions)[0].environment : item.name], "HELMR_BUILD_POLICY_PATH") &&
      !contains([for item in jsondecode(aws_ecs_task_definition.migration.container_definitions)[0].environment : item.name], "HELMR_PLATFORM_STORE_URI")
    )
    error_message = "Dispatcher and migration must not receive build policy configuration."
  }

  assert {
    condition = (
      strcontains(aws_iam_role_policy.control_task.policy, "${var.platform_store_bucket_arn}/objects/sha256/*") &&
      !strcontains(aws_iam_role_policy.control_task.policy, "PublishRetainedArtifacts") &&
      !strcontains(aws_iam_role_policy.control_task.policy, "ReadRetainedArtifacts") &&
      !strcontains(aws_iam_role_policy.control_task.policy, "${var.platform_store_bucket_arn}/control/runtime") &&
      aws_ecs_task_definition.migration.task_role_arn == aws_iam_role.migration_task.arn &&
      aws_iam_role.migration_task.name == "helmr-test-migration-task"
    )
    error_message = "Control IAM must read only runtime objects, exclude Manager storage, rollout lineage, and retained artifacts, and not leak to migration."
  }
}

run "execution_roles_are_pull_only_for_the_exact_control_repository" {
  command = plan

  assert {
    condition = (
      strcontains(aws_iam_role_policy.control_execution.policy, var.control_image_repository_arn) &&
      strcontains(aws_iam_role_policy.dispatcher_execution.policy, var.control_image_repository_arn) &&
      strcontains(aws_iam_role_policy.control_execution.policy, "ecr:BatchGetImage") &&
      strcontains(aws_iam_role_policy.dispatcher_execution.policy, "ecr:GetDownloadUrlForLayer") &&
      !strcontains(aws_iam_role_policy.control_execution.policy, "ecr:PutImage") &&
      !strcontains(aws_iam_role_policy.dispatcher_execution.policy, "ecr:PutImage") &&
      !strcontains(aws_iam_role_policy.control_execution.policy, "ecr:Delete") &&
      !strcontains(aws_iam_role_policy.dispatcher_execution.policy, "ecr:Delete")
    )
    error_message = "ECS execution roles must have pull-only access to the exact Control release repository."
  }
}

run "workspace_image_cache_is_exact_and_control_only" {
  command = plan

  assert {
    condition = (
      { for item in jsondecode(aws_ecs_task_definition.control.container_definitions)[1].environment : item.name => item.value }.HELMR_IMAGE_CACHE_REGISTRY_AUTHORITY == "000000000000.dkr.ecr.us-east-1.amazonaws.com" &&
      { for item in jsondecode(aws_ecs_task_definition.control.container_definitions)[1].environment : item.name => item.value }.HELMR_IMAGE_CACHE_REPOSITORY_PREFIX == "helmr-test/image-cache" &&
      { for item in jsondecode(aws_ecs_task_definition.control.container_definitions)[1].environment : item.name => item.value }.HELMR_IMAGE_CACHE_ROLE_ARN == "arn:aws:iam::000000000000:role/helmr-test-image-cache" &&
      { for item in jsondecode(aws_ecs_task_definition.control.container_definitions)[1].environment : item.name => item.value }.HELMR_IMAGE_CACHE_REPOSITORY_ARN_PREFIX == "arn:aws:ecr:us-east-1:000000000000:repository/helmr-test/image-cache/" &&
      !strcontains({ for item in jsondecode(aws_ecs_task_definition.control.container_definitions)[1].environment : item.name => item.value }.HELMR_WORKER_GROUPS, "instance_role_arn") &&
      !contains([for item in jsondecode(aws_ecs_task_definition.dispatcher.container_definitions)[0].environment : item.name], "HELMR_IMAGE_CACHE_ROLE_ARN") &&
      !contains([for item in jsondecode(aws_ecs_task_definition.migration.container_definitions)[0].environment : item.name], "HELMR_IMAGE_CACHE_ROLE_ARN")
    )
    error_message = "only Control must receive the complete exact Execution image-cache configuration"
  }

  assert {
    condition = (
      strcontains(aws_iam_role_policy.control_task.policy, "ProvisionExecutionImageCache") &&
      strcontains(aws_iam_role_policy.control_task.policy, "arn:aws:ecr:us-east-1:000000000000:repository/helmr-test/image-cache/environments/*") &&
      strcontains(aws_iam_role_policy.control_task.policy, "ecr:CreateRepository") &&
      strcontains(aws_iam_role_policy.control_task.policy, "ecr:SetRepositoryPolicy") &&
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
    error_message = "Control provisioning and Worker cache-role authority must be bounded to the Environment repository namespace"
  }
}
