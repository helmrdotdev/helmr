resource "terraform_data" "private_contract" {
  lifecycle {
    precondition {
      condition     = alltrue([for cidr in concat([var.controlplane_vpc_cidr, var.execution_vpc_cidr], tolist(var.company_client_ipv4_cidrs)) : contains(var.worker_network_blocked_ipv4_cidrs, cidr)])
      error_message = "Guest management isolation requires control-plane, execution and every routed company prefix in worker_network_blocked_ipv4_cidrs before private routing is enabled."
    }
    precondition {
      condition     = var.controlplane_image_repository_arn != null && can(regex("^[0-9]{12}\\.dkr\\.ecr\\.${var.aws_region}\\.", module.release_artifacts.controlplane_image))
      error_message = "Private startup requires a regional ECR image and its exact repository ARN. Mirror the release before deployment."
    }
    precondition {
      condition     = contains(["none", "resend"], var.email_provider)
      error_message = "The private template supports disabled email or Resend through the approved HTTPS exit. SMTP needs a separately reviewed private service path."
    }
    precondition {
      condition     = can(regex("^https://[^/:]+:${var.clickhouse_port}/?$", var.clickhouse_url)) || (var.clickhouse_port == 443 && can(regex("^https://[^/:]+/?$", var.clickhouse_url)))
      error_message = "clickhouse_url must use HTTPS and the declared private ClickHouse port."
    }
  }
}
