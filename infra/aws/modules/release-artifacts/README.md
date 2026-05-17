# Helmr Release Artifacts Module

This module resolves immutable Helmr release artifacts for AWS deployments.
Terraform/OpenTofu does not build images; it reads a release manifest and passes the resulting
control image and worker AMI into the infrastructure modules.

Default manifest URL:

```text
https://github.com/helmrdotdev/helmr/releases/download/<helmr_version>/aws-artifacts.json
```

Expected manifest shape:

```json
{
  "control_image": "ghcr.io/helmrdotdev/helmr-control@sha256:...",
  "worker_amis": {
    "us-east-1": "ami-0123456789abcdef0"
  }
}
```

## Usage

```hcl
module "release_artifacts" {
  source = "../../modules/release-artifacts"

  helmr_version      = "vX.Y.Z"
  aws_region         = "us-east-1"
  resolve_worker_ami = true
}

module "control" {
  source = "../../modules/control"

  control_image = module.release_artifacts.control_image
}

module "worker" {
  source = "../../modules/worker"

  ami_id = module.release_artifacts.worker_ami_id
}
```

Use `control_image_override` and `worker_ami_id_override` for custom builds or forks. When
`resolve_worker_ami` is false, the worker AMI may be null.
