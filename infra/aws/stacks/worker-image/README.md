# Worker image stack

This stack builds and distributes the Worker AMI from exact content-addressed
host and runtime bundles. It does not stage or embed Managed Runtime,
Manager, toolchain, or build-policy artifacts.

```sh
nix develop .#infra
tofu -chdir=infra/aws/stacks/worker-image init
tofu -chdir=infra/aws/stacks/worker-image apply \
  -var="aws_region=us-east-1" \
  -var="name=helmr-worker" \
  -var="host_artifacts_bundle_s3_uri=s3://..." \
  -var="host_artifacts_bundle_digest=sha256:..." \
  -var="runtime_artifacts_bundle_s3_uri=s3://..." \
  -var="runtime_artifacts_bundle_digest=sha256:..."
```
