# Admission also checks this relation against each authenticated host report.
locals {
  control_network_ranges = {
    business = [for cidr in var.worker_network_private_business_cidrs : {
      first = sum([for index, octet in split(".", cidrhost(cidr, 0)) : tonumber(octet) * pow(256, 3 - index)])
      last  = sum([for index, octet in split(".", cidrhost(cidr, -1)) : tonumber(octet) * pow(256, 3 - index)])
    }]
    blocked = [for cidr in var.worker_network_blocked_ipv4_cidrs : {
      first = sum([for index, octet in split(".", cidrhost(cidr, 0)) : tonumber(octet) * pow(256, 3 - index)])
      last  = sum([for index, octet in split(".", cidrhost(cidr, -1)) : tonumber(octet) * pow(256, 3 - index)])
    }]
  }
  retained_network_ranges = { for key, spec in local.worker_generation_specs : key => {
    business = [for cidr in spec.generation_inputs.supply.network.private_business_cidrs : {
      first = sum([for index, octet in split(".", cidrhost(cidr, 0)) : tonumber(octet) * pow(256, 3 - index)])
      last  = sum([for index, octet in split(".", cidrhost(cidr, -1)) : tonumber(octet) * pow(256, 3 - index)])
    }]
    blocked = [for cidr in spec.generation_inputs.supply.network.blocked_ipv4_cidrs : {
      first = sum([for index, octet in split(".", cidrhost(cidr, 0)) : tonumber(octet) * pow(256, 3 - index)])
      last  = sum([for index, octet in split(".", cidrhost(cidr, -1)) : tonumber(octet) * pow(256, 3 - index)])
    }]
  } }
}
resource "terraform_data" "network_installation_contract" {
  lifecycle {
    precondition {
      condition = alltrue(flatten([for generation in values(local.retained_network_ranges) : [
        for business in generation.business : alltrue([
          for address in concat([business.first], [for ceiling in local.control_network_ranges.business : ceiling.last + 1 if ceiling.last >= business.first && ceiling.last < business.last]) :
          anytrue([for ceiling in local.control_network_ranges.business : ceiling.first <= address && ceiling.last >= address])
        ])
      ]]))
      error_message = "Every retained generation's private business ceiling must be a subset of the control-plane ceiling. Drain incompatible generations before changing installation restrictions."
    }
    precondition {
      condition = alltrue(flatten([for generation in values(local.retained_network_ranges) : [
        for required in local.control_network_ranges.blocked : alltrue([
          for address in concat([required.first], [for blocked in generation.blocked : blocked.last + 1 if blocked.last >= required.first && blocked.last < required.last]) :
          anytrue([for blocked in generation.blocked : blocked.first <= address && blocked.last >= address])
        ])
      ]]))
      error_message = "Every retained generation must cover the control-plane mandatory deny set. Drain older permissive generations before activating narrower restrictions."
    }
  }
}
