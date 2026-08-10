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
  name                                = "helmr-test"
  parent_image                        = "ami-00000000000000000"
  source_ref                          = "0123456789abcdef0123456789abcdef01234567"
  host_artifacts_bundle_s3_uri        = "s3://helmr-test/host/worker-host.tar"
  host_artifacts_bundle_object_arn    = "arn:aws:s3:::helmr-test/host/worker-host.tar"
  host_artifacts_bundle_digest        = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
  host_artifacts_manifest_digest      = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
  runtime_artifacts_bundle_s3_uri     = "s3://helmr-test/runtime/worker-runtime.tar"
  runtime_artifacts_bundle_object_arn = "arn:aws:s3:::helmr-test/runtime/worker-runtime.tar"
  runtime_artifacts_bundle_digest     = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  runtime_artifacts_manifest_digest   = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}

run "image_installs_verified_worker_artifacts" {
  command = plan

  assert {
    condition = (
      strcontains(aws_imagebuilder_component.worker.data, "/usr/local/bin/helmr-worker") &&
      strcontains(aws_imagebuilder_component.worker.data, "/usr/local/bin/firecracker") &&
      strcontains(aws_imagebuilder_component.worker.data, "gpgv") &&
      strcontains(aws_imagebuilder_component.worker.data, "mksquashfs") &&
      strcontains(aws_imagebuilder_component.worker.data, "aws_cli_version=2.31.39") &&
      strcontains(aws_imagebuilder_component.worker.data, "https://awscli.amazonaws.com/awscli-exe-linux-x86_64-") &&
      strcontains(aws_imagebuilder_component.worker.data, "5a2ad4e63f8f687d735f8e7a132b3622a1cf08fa884c53e3423c9b83a3c0d663")
    )
    error_message = "Worker AMI must contain the host executables and Platform acquisition tools."
  }

  assert {
    condition = (
      strcontains(aws_imagebuilder_component.worker.data, "s3://helmr-test/runtime/worker-runtime.tar") &&
      strcontains(aws_imagebuilder_component.worker.data, "s3://helmr-test/host/worker-host.tar") &&
      strcontains(aws_imagebuilder_component.worker.data, "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd") &&
      strcontains(aws_imagebuilder_component.worker.data, "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc") &&
      strcontains(aws_imagebuilder_component.worker.data, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb") &&
      strcontains(aws_imagebuilder_component.worker.data, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
    )
    error_message = "Worker image build must install and verify the exact runtime artifact bundle without rebuilding it."
  }

  assert {
    condition = (
      strcontains(aws_imagebuilder_component.worker.data, "source_commit '0123456789abcdef0123456789abcdef01234567'") &&
      strcontains(aws_imagebuilder_component.worker.data, "/usr/local/bin/helmr-worker version")
    )
    error_message = "Worker image build must inject and verify the exact source commit as the Worker binary identity."
  }

  assert {
    condition = one(
      one(aws_imagebuilder_distribution_configuration.worker.distribution).ami_distribution_configuration
    ).ami_tags.HelmrHostArtifactsDigest == "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
    error_message = "Worker image build and AMI provenance must bind the exact host artifact manifest digest."
  }

  assert {
    condition = one(
      one(aws_imagebuilder_distribution_configuration.worker.distribution).ami_distribution_configuration
    ).ami_tags.HelmrHostBundleDigest == "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
    error_message = "Worker AMI provenance must bind the exact host artifact bundle digest."
  }

  assert {
    condition = one(
      one(aws_imagebuilder_distribution_configuration.worker.distribution).ami_distribution_configuration
    ).ami_tags.HelmrRuntimeArtifactsDigest == "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    error_message = "Worker image build and AMI provenance must bind the exact runtime artifact manifest digest."
  }

  assert {
    condition = one(
      one(aws_imagebuilder_distribution_configuration.worker.distribution).ami_distribution_configuration
    ).ami_tags.HelmrRuntimeBundleDigest == "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    error_message = "Worker AMI provenance must bind the exact runtime artifact bundle digest."
  }

  assert {
    condition = (
      strcontains(aws_iam_role_policy.build_artifacts.policy, "arn:aws:s3:::helmr-test/host/worker-host.tar") &&
      strcontains(aws_iam_role_policy.build_artifacts.policy, "arn:aws:s3:::helmr-test/runtime/worker-runtime.tar")
    )
    error_message = "Image Builder must only receive access to the exact host and runtime artifact bundle objects."
  }
}
