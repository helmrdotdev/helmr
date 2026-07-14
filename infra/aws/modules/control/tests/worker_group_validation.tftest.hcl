mock_provider "aws" {}
mock_provider "random" {}

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
        milli_cpu         = 4000, memory_bytes = 8589934592, workload_disk_bytes = 34359738368, scratch_bytes = 34359738368
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
        milli_cpu         = 4000, memory_bytes = 8589934592, workload_disk_bytes = 34359738368, scratch_bytes = 34359738368
        build_cache_bytes = 0, artifact_cache_bytes = 0, vm_slots = 2, build_executors = -1
      }
    }]
  }

  expect_failures = [terraform_data.bootstrap_preconditions]
}
