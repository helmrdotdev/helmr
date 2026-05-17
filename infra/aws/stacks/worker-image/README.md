# Helmr Worker Image Stack

This stack creates an EC2 Image Builder pipeline for the worker AMI required by the dev stack.

Initialize with an existing S3 backend:

```sh
terraform init \
  -backend-config="bucket=<state-bucket>" \
  -backend-config="region=<state-region>"
```

Apply the pipeline:

```sh
terraform apply \
  -var="aws_region=us-east-1" \
  -var="name=helmr-smoke-image" \
  -var="source_ref=<branch-or-commit>"
```

Start the build:

```sh
aws imagebuilder start-image-pipeline-execution \
  --image-pipeline-arn "$(terraform output -raw image_pipeline_arn)"
```

After the build completes, pass the produced AMI ID to `stacks/dev` as `worker_ami_id`.
