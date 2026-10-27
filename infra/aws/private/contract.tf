resource "terraform_data" "private_contract" {
  lifecycle {
    precondition {
      condition = alltrue(flatten([
        for generation in values(local.generation_network_ranges) : [
          for required in local.required_management_ranges : alltrue([
            for address in concat([required.first], [for blocked in generation.blocked : blocked.last + 1 if blocked.last >= required.first && blocked.last < required.last]) :
            anytrue([for blocked in generation.blocked : blocked.first <= address && blocked.last >= address])
          ])
        ]
      ]))
      error_message = "Every current and retained generation must cover control-plane, execution and globally routed company prefixes with absolute destination denies. RFC1918 company routes remain subject to the explicit business ceiling."

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

locals {
  # Integer intervals allow semantic IPv4-set coverage, including adjacent CIDRs.
  management_prefixes = concat([var.controlplane_vpc_cidr, var.execution_vpc_cidr], [for cidr in var.company_client_ipv4_cidrs : cidr if !anytrue([
    for private in ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"] : try(tonumber(split("/", cidr)[1]) >= tonumber(split("/", private)[1]) && cidrhost(format("%s/%s", split("/", cidr)[0], split("/", private)[1]), 0) == split("/", private)[0], false)
  ])])
  required_management_ranges = [for cidr in local.management_prefixes : {
    first = sum([for index, octet in split(".", cidrhost(cidr, 0)) : tonumber(octet) * pow(256, 3 - index)])
    last  = sum([for index, octet in split(".", cidrhost(cidr, -1)) : tonumber(octet) * pow(256, 3 - index)])
  }]
  generation_network_ranges = { for key, spec in local.worker_generation_specs : key => {
    blocked = [for cidr in spec.generation_inputs.supply.network.blocked_ipv4_cidrs : {
      first = sum([for index, octet in split(".", cidrhost(cidr, 0)) : tonumber(octet) * pow(256, 3 - index)])
      last  = sum([for index, octet in split(".", cidrhost(cidr, -1)) : tonumber(octet) * pow(256, 3 - index)])
    }]
  } }
}
