# Worker image stack

This stack builds and distributes the Worker AMI from one exact Git ref or
optional exact S3 git bundle. It does not stage or embed Managed Runtime,
Manager, toolchain, or build-policy artifacts.

```sh
nix develop .#infra
tofu -chdir=infra/aws/stacks/worker-image init
tofu -chdir=infra/aws/stacks/worker-image apply \
  -var="aws_region=us-east-1" \
  -var="name=helmr-worker" \
  -var="source_ref=<exact-tag-or-commit>"
```
