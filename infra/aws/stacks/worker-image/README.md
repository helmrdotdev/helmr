# Worker image stack

This stack builds and distributes the Worker AMI from exact content-addressed
host and runtime bundles. Deployment bundles are produced independently on the
developer or CI runner and are not Worker-image inputs.

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

`permissions_boundary_arn` is an optional caller-owned IAM ceiling for the image
builder role. It must be a customer-managed policy in the caller account and
partition. The stack forwards it to the generic module; the module's resource
grants remain independent and no managed policy is created for this input.
