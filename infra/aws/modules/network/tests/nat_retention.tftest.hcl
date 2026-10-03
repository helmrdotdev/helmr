mock_provider "aws" {
  mock_data "aws_availability_zones" {
    defaults = { names = ["us-east-1a", "us-east-1b"] }
  }
  mock_data "aws_region" {
    defaults = { region = "us-east-1" }
  }
}

variables {
  name           = "helmr-test"
  retain_nat_eip = true
}

run "online" {
  command = apply
}

run "off" {
  command = apply
  variables { enable_nat_gateway = false }
  assert {
    condition     = length(aws_eip.nat) == 1 && length(aws_nat_gateway.main) == 0 && length(aws_route.private_internet) == 0
    error_message = "NAT shutdown must retain an EIP while removing NAT and its default route."
  }
  assert {
    condition     = aws_vpc.main.id != null && length(aws_vpc_endpoint.s3) == 1
    error_message = "NAT shutdown must preserve the VPC and S3 gateway endpoint."
  }
}

run "reopen" {
  command = apply
  assert {
    condition     = aws_nat_gateway.main[0].allocation_id == aws_eip.nat[0].id
    error_message = "Recreated NAT must use the retained allocation."
  }
}

run "retire" {
  command = apply
  variables {
    enable_nat_gateway = false
    retain_nat_eip     = false
  }
  assert {
    condition     = length(aws_eip.nat) == 0 && length(aws_nat_gateway.main) == 0 && length(aws_route.private_internet) == 0
    error_message = "Explicit final retirement must remove the allocation."
  }
}
