mock_provider "aws" {
  mock_data "aws_region" {
    defaults = {
      region = "us-east-1"
    }
  }

  mock_resource "aws_launch_template" {
    defaults = {
      id = "lt-00000000000000000"
    }
  }
}

variables {
  name               = "helmr-test-run"
  worker_group_id    = "run-workers"
  vpc_id             = "vpc-00000000000000000"
  subnet_ids         = ["subnet-00000000000000000"]
  ami_id             = "ami-00000000000000000"
  worker_control_url = "https://control.example.test"
  cas_uri            = "s3://helmr-test-cas"
  cas_bucket_arn     = "arn:aws:s3:::helmr-test-cas"
  kms_key_arn        = "arn:aws:kms:us-east-1:111122223333:key/00000000-0000-0000-0000-000000000000"
  min_size           = 0
  max_size           = 1
  secret_arns = {
    checkpoint_encryption_key = "arn:aws:secretsmanager:us-east-1:111122223333:secret:checkpoint"
  }
}

run "controller_owns_protected_capacity" {
  command = plan

  variables {
    launch_lifecycle_heartbeat_timeout_seconds = 321
  }

  assert {
    condition     = aws_autoscaling_group.worker.protect_from_scale_in
    error_message = "managed controller capacity must start protected from scale in"
  }

  assert {
    condition     = strcontains(base64decode(aws_launch_template.worker.user_data), "HELMR_WORKER_DISK_RESERVE_MIB=1024")
    error_message = "worker user data must pin the disk reserve used by certified capacity math"
  }

  assert {
    condition     = strcontains(base64decode(aws_launch_template.worker.user_data), "launch_timeout='321'") && strcontains(base64decode(aws_launch_template.worker.user_data), "drain-complete") && strcontains(base64decode(aws_launch_template.worker.user_data), "ABANDON")
    error_message = "worker lifecycle handling must bound launch readiness and bypass repeated drain after durable local completion"
  }
}

run "controller_requires_lifecycle_hooks" {
  command = plan

  variables {
    enable_lifecycle_hooks = false
  }

  expect_failures = [aws_autoscaling_group.worker]
}

run "explicit_disk_must_exceed_reserve" {
  command = plan

  variables {
    worker_disk_mib         = 1024
    worker_disk_reserve_mib = 1024
  }

  expect_failures = [terraform_data.network_preconditions]
}
