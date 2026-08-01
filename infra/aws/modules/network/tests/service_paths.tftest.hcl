mock_provider "aws" {
  mock_data "aws_availability_zones" {
    defaults = {
      names = ["us-east-1a", "us-east-1b", "us-east-1c"]
    }
  }

  mock_data "aws_region" {
    defaults = {
      region = "us-east-1"
    }
  }

  mock_resource "aws_vpc" {
    defaults = {
      id = "vpc-00000000000000000"
    }
  }

  mock_resource "aws_internet_gateway" {
    defaults = {
      id = "igw-00000000000000000"
    }
  }

  mock_resource "aws_nat_gateway" {
    defaults = {
      id = "nat-00000000000000000"
    }
  }

  mock_resource "aws_route_table" {
    defaults = {
      id = "rtb-00000000000000000"
    }
  }
}

variables {
  name                    = "helmr-test"
  vpc_cidr                = "10.80.0.0/16"
  availability_zone_count = 2
}

run "private_service_paths_are_exact" {
  command = plan

  assert {
    condition     = aws_vpc.main.enable_dns_support && aws_vpc.main.enable_dns_hostnames
    error_message = "the VPC must retain DNS support for the regional public service paths"
  }

  assert {
    condition = (
      aws_route.public_internet.destination_cidr_block == "0.0.0.0/0" &&
      aws_route.public_internet.gateway_id == aws_internet_gateway.main.id
    )
    error_message = "the public route table must reach the Internet only through the VPC internet gateway"
  }

  assert {
    condition = (
      one(aws_route.private_internet).destination_cidr_block == "0.0.0.0/0" &&
      one(aws_route.private_internet).nat_gateway_id == one(aws_nat_gateway.main).id
    )
    error_message = "the private route table must reach ordinary public destinations through NAT"
  }

  assert {
    condition = (
      one(aws_vpc_endpoint.s3).vpc_endpoint_type == "Gateway" &&
      one(aws_vpc_endpoint.s3).service_name == "com.amazonaws.us-east-1.s3" &&
      toset(one(aws_vpc_endpoint.s3).route_table_ids) == toset([aws_route_table.private.id]) &&
      jsondecode(one(aws_vpc_endpoint.s3).policy) == {
        Version = "2012-10-17"
        Statement = [{
          Effect    = "Allow"
          Principal = "*"
          Action    = "*"
          Resource  = "*"
        }]
      }
    )
    error_message = "same-Region S3 must use the private route table's non-narrowing Gateway Endpoint"
  }
}

run "operator_can_disable_public_nat_without_losing_s3" {
  command = plan

  variables {
    enable_nat_gateway = false
  }

  assert {
    condition     = length(aws_nat_gateway.main) == 0 && length(aws_route.private_internet) == 0
    error_message = "disabling NAT must remove the private default Internet route"
  }

  assert {
    condition = (
      length(aws_vpc_endpoint.s3) == 1 &&
      one(aws_vpc_endpoint.s3).vpc_endpoint_type == "Gateway" &&
      toset(one(aws_vpc_endpoint.s3).route_table_ids) == toset([aws_route_table.private.id])
    )
    error_message = "the S3 Gateway path must remain independently configurable from public NAT"
  }
}

run "operator_can_disable_s3_gateway" {
  command = plan

  variables {
    enable_s3_gateway_endpoint = false
  }

  assert {
    condition     = length(aws_vpc_endpoint.s3) == 0
    error_message = "the generic network module must honor the operator's explicit S3 endpoint choice"
  }
}
