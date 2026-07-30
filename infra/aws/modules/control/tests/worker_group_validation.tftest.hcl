mock_provider "aws" {}
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
  platform_store_uri           = "s3://helmr-test-runtime/objects"
  platform_store_bucket_arn    = "arn:aws:s3:::helmr-test-runtime"
  platform_store_kms_key_arn   = "arn:aws:kms:us-east-1:000000000000:key/11111111-1111-1111-1111-111111111111"
  control_image_repository_arn = "arn:aws:ecr:us-east-1:000000000000:repository/helmr-test/control-releases"
  build_policy_digest          = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}

run "worker_group_requires_a_role" {
  command = plan

  variables {
    name                         = "helmr-test"
    vpc_id                       = "vpc-0123456789abcdef0"
    private_subnet_ids           = ["subnet-0123456789abcdef0", "subnet-1123456789abcdef0"]
    public_subnet_ids            = ["subnet-2123456789abcdef0", "subnet-3123456789abcdef0"]
    worker_group_id              = "run-workers"
    control_image                = "example.invalid/helmr@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    clickhouse_url               = "https://clickhouse.example.invalid"
    github_oauth_client_id       = "test-client"
    allow_insecure_http          = true
    database_skip_final_snapshot = true
    worker_groups = [{
      id                = "run-workers", name = "Run workers", region = "us-east-1", account_id = "000000000000"
      autoscaling_group = "helmr-run", instance_profile_arn = "arn:aws:iam::000000000000:instance-profile/helmr-run"
      launch_ami_id     = "ami-0123456789abcdef0", ami_ids = ["ami-0123456789abcdef0"]
      allows_run        = false, allows_build = false
      instance_capacity = {
        milli_cpu         = 4000, memory_bytes = 8589934592, guest_ephemeral_disk_bytes = 34359738368
        build_cache_bytes = 0, artifact_cache_bytes = 0, vm_slots = 0, build_executors = 0
      }
    }]
  }

  expect_failures = [terraform_data.bootstrap_preconditions]
}

run "inactive_role_capacity_cannot_be_negative" {
  command = plan

  variables {
    name                         = "helmr-test"
    vpc_id                       = "vpc-0123456789abcdef0"
    private_subnet_ids           = ["subnet-0123456789abcdef0", "subnet-1123456789abcdef0"]
    public_subnet_ids            = ["subnet-2123456789abcdef0", "subnet-3123456789abcdef0"]
    worker_group_id              = "run-workers"
    control_image                = "example.invalid/helmr@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    clickhouse_url               = "https://clickhouse.example.invalid"
    github_oauth_client_id       = "test-client"
    allow_insecure_http          = true
    database_skip_final_snapshot = true
    worker_groups = [{
      id                = "run-workers", name = "Run workers", region = "us-east-1", account_id = "000000000000"
      autoscaling_group = "helmr-run", instance_profile_arn = "arn:aws:iam::000000000000:instance-profile/helmr-run"
      launch_ami_id     = "ami-0123456789abcdef0", ami_ids = ["ami-0123456789abcdef0"]
      allows_run        = true, allows_build = false
      instance_capacity = {
        milli_cpu         = 4000, memory_bytes = 8589934592, guest_ephemeral_disk_bytes = 34359738368
        build_cache_bytes = 0, artifact_cache_bytes = 0, vm_slots = 2, build_executors = -1
      }
    }]
  }

  expect_failures = [terraform_data.bootstrap_preconditions]
}
