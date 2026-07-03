terraform {
  required_version = ">= 1.10"

  backend "s3" {
    key          = "helmr/stacks/dev/terraform.tfstate"
    use_lockfile = true
  }

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.44"
    }
    clickhouse = {
      source  = "ClickHouse/clickhouse"
      version = ">= 3.17.3"
    }
  }
}

provider "aws" {
  region = var.aws_region

  default_tags {
    tags = local.tags
  }
}

provider "clickhouse" {
  organization_id = var.clickhouse_organization_id
}
