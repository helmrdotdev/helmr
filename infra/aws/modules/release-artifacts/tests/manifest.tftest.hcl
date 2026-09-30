mock_provider "http" {
  mock_data "http" {
    defaults = {
      status_code   = 200
      response_body = "{\"schema\":\"helmr.aws-release.v0\",\"controlplaneImage\":\"111122223333.dkr.ecr.us-east-1.amazonaws.com/helmr@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"platformRelease\":{},\"workerImage\":{\"amis\":{\"us-east-1\":\"ami-00000000000000000\"}}}"
    }
  }
}

variables {
  helmr_version      = "v0.0.0-test"
  aws_region         = "us-east-1"
  resolve_worker_ami = true
}

run "resolves_current_release_manifest" {
  command = plan

  assert {
    condition     = output.controlplane_image == "111122223333.dkr.ecr.us-east-1.amazonaws.com/helmr@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    error_message = "controlplaneImage must resolve from the current release manifest contract"
  }

  assert {
    condition     = output.worker_ami_id == "ami-00000000000000000"
    error_message = "workerImage.amis must resolve for aws_region from the current release manifest contract"
  }
}

run "release_tag_accepts_stable" {
  command = plan
  variables { helmr_version = "v1.2.3" }
}

run "release_tag_accepts_prerelease" {
  command = plan
  variables { helmr_version = "v1.2.3-rc.1" }
}

run "release_tag_rejects_latest" {
  command = plan
  variables { helmr_version = "latest" }
  expect_failures = [var.helmr_version]
}

run "release_tag_rejects_range" {
  command = plan
  variables { helmr_version = ">=1.2.3" }
  expect_failures = [var.helmr_version]
}

run "release_tag_rejects_missing_prefix" {
  command = plan
  variables { helmr_version = "1.2.3" }
  expect_failures = [var.helmr_version]
}

run "release_tag_rejects_missing_patch" {
  command = plan
  variables { helmr_version = "v1.2" }
  expect_failures = [var.helmr_version]
}

run "release_tag_rejects_leading_zero" {
  command = plan
  variables { helmr_version = "v01.2.3" }
  expect_failures = [var.helmr_version]
}

run "release_tag_rejects_prerelease_leading_zero" {
  command = plan
  variables { helmr_version = "v1.2.3-01" }
  expect_failures = [var.helmr_version]
}

run "release_tag_rejects_build_metadata" {
  command = plan
  variables { helmr_version = "v1.2.3+build.1" }
  expect_failures = [var.helmr_version]
}

run "release_tag_rejects_trailing_text" {
  command = plan
  variables { helmr_version = "v1.2.3junk" }
  expect_failures = [var.helmr_version]
}
