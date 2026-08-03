terraform {
  required_version = ">= 1.10"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.44"
    }
  }
}

data "aws_region" "current" {}
data "aws_partition" "current" {}
data "aws_vpc" "selected" {
  id = var.vpc_id
}
