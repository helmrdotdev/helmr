mock_provider "aws" {
  mock_data "aws_region" {
    defaults = {
      region = "us-east-1"
    }
  }

  mock_resource "aws_imagebuilder_component" {
    defaults = {
      arn = "arn:aws:imagebuilder:us-east-1:000000000000:component/helmr-test-worker/0.1.0/1"
    }
  }

  mock_resource "aws_imagebuilder_image_recipe" {
    defaults = {
      arn = "arn:aws:imagebuilder:us-east-1:000000000000:image-recipe/helmr-test-worker/0.1.0"
    }
  }

  mock_resource "aws_imagebuilder_infrastructure_configuration" {
    defaults = {
      arn = "arn:aws:imagebuilder:us-east-1:000000000000:infrastructure-configuration/helmr-test-worker"
    }
  }

  mock_resource "aws_imagebuilder_distribution_configuration" {
    defaults = {
      arn = "arn:aws:imagebuilder:us-east-1:000000000000:distribution-configuration/helmr-test-worker"
    }
  }
}

variables {
  name         = "helmr-test"
  parent_image = "ami-00000000000000000"
  source_ref   = "0123456789abcdef0123456789abcdef01234567"
}

run "image_contains_worker_tools_without_platform_artifacts" {
  command = plan

  assert {
    condition = (
      strcontains(aws_imagebuilder_component.worker.data, "/usr/local/bin/helmr-worker") &&
      strcontains(aws_imagebuilder_component.worker.data, "/usr/local/bin/firecracker") &&
      strcontains(aws_imagebuilder_component.worker.data, "/usr/local/bin/buildkitd") &&
      strcontains(aws_imagebuilder_component.worker.data, "gpgv") &&
      strcontains(aws_imagebuilder_component.worker.data, "mksquashfs")
    )
    error_message = "Worker AMI must contain the host executables and Platform acquisition tools."
  }

  assert {
    condition = (
      !strcontains(aws_imagebuilder_component.worker.data, "runtime-release") &&
      !strcontains(aws_imagebuilder_component.worker.data, "manager-release") &&
      !strcontains(aws_imagebuilder_component.worker.data, "build-policy.json") &&
      !strcontains(aws_imagebuilder_component.worker.data, "/objects/sha256/")
    )
    error_message = "Worker AMI must not bake a Runtime, Manager, toolchain, or build policy."
  }
}
