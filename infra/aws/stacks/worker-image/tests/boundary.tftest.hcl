mock_provider "aws" {
  mock_data "aws_caller_identity" { defaults = { account_id = "000000000000" } }
  mock_data "aws_partition" { defaults = { partition = "aws" } }
  mock_data "aws_region" {
    defaults = {
      region = "us-east-1"
    }
  }

  mock_resource "aws_imagebuilder_component" {
    defaults = {
      arn = "arn:aws:imagebuilder:us-east-1:000000000000:component/helmr-test-worker/1.0.0/1"
    }
  }

  mock_resource "aws_imagebuilder_image_recipe" {
    defaults = {
      arn = "arn:aws:imagebuilder:us-east-1:000000000000:image-recipe/helmr-test-worker/1.0.0"
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
  aws_region                          = "us-east-1"
  name                                = "helmr-test"
  parent_image                        = "ami-00000000000000000"
  root_volume_encrypted               = false
  host_artifacts_bundle_s3_uri        = "s3://helmr-test/host/worker-host.tar"
  host_artifacts_bundle_object_arn    = "arn:aws:s3:::helmr-test/host/worker-host.tar"
  host_artifacts_bundle_digest        = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
  host_artifacts_manifest_digest      = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
  runtime_artifacts_bundle_s3_uri     = "s3://helmr-test/runtime/worker-runtime.tar"
  runtime_artifacts_bundle_object_arn = "arn:aws:s3:::helmr-test/runtime/worker-runtime.tar"
  runtime_artifacts_bundle_digest     = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  runtime_artifacts_manifest_digest   = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}

run "caller_ceiling_composition" {
  command = plan
  variables {
    name                     = "helmr-test-image"
    permissions_boundary_arn = "arn:aws:iam::000000000000:policy/workload"
  }
}
