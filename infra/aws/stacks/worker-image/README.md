# Helmr Worker Image Stack

This stack creates an EC2 Image Builder pipeline for the worker AMI required by the dev stack.

Run commands from the repository Nix infra shell:

```sh
nix develop .#infra
```

Initialize with an existing S3 backend:

```sh
tofu init \
  -backend-config="bucket=<state-bucket>" \
  -backend-config="region=<state-region>"
```

Apply the pipeline:

```sh
tofu apply \
  -var="aws_region=us-east-1" \
  -var="name=helmr-smoke-image" \
  -var="source_ref=<branch-or-commit>"
```

Start the build:

```sh
aws imagebuilder start-image-pipeline-execution \
  --image-pipeline-arn "$(tofu output -raw image_pipeline_arn)"
```

After the build completes, pass the produced AMI ID to `stacks/dev` as `worker_ami_id`.

## Official release AMIs

Official release AMIs are public, unencrypted, and distributed from one Image Builder pipeline run
to the supported customer regions. AWS does not allow public AMIs backed by encrypted snapshots, so
`root_volume_encrypted` must be false when `ami_public` is true.

The release workflow runs this path automatically when `worker_amis_json` is left as `{}`. For a
local repair or one-off build, use the repository script from the root so the same flow can collect
the release manifest input:

```sh
export AWS_REGION=us-east-1
export STATE_KEY=helmr/stacks/release-worker-image/terraform.tfstate
export WORKER_IMAGE_NAME=helmr-release-image
export WORKER_IMAGE_SOURCE_REF=<release-commit-or-tag>
export WORKER_IMAGE_VERSION=0.1.1
export WORKER_IMAGE_DISTRIBUTION_REGIONS=us-east-1,us-west-2,ap-northeast-1
export WORKER_IMAGE_AMI_PUBLIC=true
export WORKER_IMAGE_ROOT_VOLUME_ENCRYPTED=false

scripts/aws-dev-smoke.sh worker-image-init
scripts/aws-dev-smoke.sh worker-image-apply
scripts/aws-dev-smoke.sh worker-image-start
scripts/aws-dev-smoke.sh worker-image-wait
scripts/aws-dev-smoke.sh worker-image-amis
```

Pass the JSON printed by `worker-image-amis` to the release workflow's `worker_amis_json` input for
the same Helmr version when overriding the automatic AMI build.
