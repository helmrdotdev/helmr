terraform {
  required_version = ">= 1.10"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.44"
    }
    clickhouse = {
      source  = "ClickHouse/clickhouse"
      version = ">= 3.17.3"
    }
    random = {
      source  = "hashicorp/random"
      version = ">= 3.7"
    }
  }
}

data "aws_region" "current" {}
