terraform {
  required_version = ">= 1.10"

  backend "s3" {
    key          = "helmr/stacks/release-build/terraform.tfstate"
    use_lockfile = true
  }

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.0"
    }
  }
}

data "aws_region" "current" {}
data "aws_caller_identity" "current" {}
