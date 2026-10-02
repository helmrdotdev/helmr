# Worker image stack

This stack defines an EC2 Image Builder pipeline that installs the exact host
and runtime bundles from a verified public Helmr release. Common releases do not
publish regional Worker AMIs. Applying this stack creates the pipeline; starting
an image build is a separate operation. Deployment bundles are not image inputs.

## Prepare the inputs

Use a public Product checkout at the release's source commit, OpenTofu and AWS
CLI with the operator's native AWS authentication. This checkout supplies the
tools through `nix develop .#infra`. No private deployment repository is required.
First verify the complete signed release index and downloaded assets under the
[release artifact requirements](../../../../packages/web/src/content/docs/self-hosting/requirements.md#release-artifacts).
Keep the index digest and the exact version for later image provenance.

The image build role must be able to read the two exact S3 objects. Public S3
release objects can be read directly; otherwise copy the already verified bytes
to an operator-owned bucket without changing their digests. Supply exact object
ARNs and, for SSE-KMS objects, their KMS key ARNs. Do not select artifacts by a
mutable channel during the build.

Create a local `worker-image.tfvars` with all required fields below, replacing
placeholders with the verified values. Bundle digests are the signed index's
`assets["worker-host-artifacts.tar"].digest` and
`assets["runtime-artifacts.tar"].digest`; manifest digests are its corresponding
`worker-host-artifacts.json` and `runtime-artifacts.json` asset digests.

```hcl
aws_region = "us-east-1"
name       = "my-helmr-worker"

host_artifacts_bundle_s3_uri     = "s3://BUCKET/EXACT_PREFIX/worker-host-artifacts.tar"
host_artifacts_bundle_object_arn = "arn:aws:s3:::BUCKET/EXACT_PREFIX/worker-host-artifacts.tar"
host_artifacts_bundle_digest     = "sha256:VERIFIED_HOST_BUNDLE_HASH"
host_artifacts_manifest_digest   = "sha256:VERIFIED_HOST_MANIFEST_HASH"
runtime_artifacts_bundle_s3_uri     = "s3://BUCKET/EXACT_PREFIX/runtime-artifacts.tar"
runtime_artifacts_bundle_object_arn = "arn:aws:s3:::BUCKET/EXACT_PREFIX/runtime-artifacts.tar"
runtime_artifacts_bundle_digest     = "sha256:VERIFIED_RUNTIME_BUNDLE_HASH"
runtime_artifacts_manifest_digest   = "sha256:VERIFIED_RUNTIME_MANIFEST_HASH"

parent_image          = "ami-CONCRETE_UBUNTU_24_04_AMD64_ID"
instance_types        = ["c8i.xlarge"]
subnet_id             = "subnet-OPERATOR_BUILD_SUBNET"
security_group_ids    = ["sg-OPERATOR_BUILD_SECURITY_GROUP"]
distribution_regions  = ["us-east-1"]
ami_public            = false
root_volume_encrypted = true
root_volume_size_gb    = 24
```

Select an available Canonical Ubuntu 24.04 amd64 parent in the build Region
(owner `099720109477`) and pin its concrete AMI ID. The build network needs DNS
and outbound access to Ubuntu package repositories, AWS CLI distribution, S3,
SSM and Image Builder services. No inbound SSH is required. A public subnet needs
public address assignment and an internet route; a private subnet needs suitable
outbound connectivity. An install-only build does not prove runtime KVM support.

`permissions_boundary_arn` is an optional caller-owned IAM ceiling in the same
account and partition. It does not replace the module's resource grants.

## Plan and build

Operators who already compose infrastructure can instead use the public
`infra/aws/modules/worker-image` module directly in their own root, with their
chosen durable backend and build network. Set the provider Region in that root
and pass the same image inputs (all fields below `aws_region` above) to the
module. Expose its pipeline/image-definition outputs and use the same native
start, observation, compatibility and cleanup procedure below. The standalone
stack is a convenience composition, not a prerequisite for using the module.

The standalone stack uses an operator-owned S3 state backend. Prepare a durable state bucket
and a unique key per independently retained image stack; never reuse another
stack's state. From the public checkout, initialize with those real inputs:

```sh
nix develop .#infra
tofu -chdir=infra/aws/stacks/worker-image init \
  -backend-config="bucket=YOUR_STATE_BUCKET" \
  -backend-config="region=YOUR_STATE_REGION" \
  -backend-config="key=helmr/worker-images/YOUR_IMAGE_NAME.tfstate"
tofu -chdir=infra/aws/stacks/worker-image plan \
  -var-file=/absolute/path/worker-image.tfvars -out=/absolute/path/image.tfplan
```

Inspect the saved plan and apply that unchanged file under your AWS change and
spend authority. This can create IAM and Image Builder resources, and Image Builder can create its account-wide service-linked role during
initial use or recreation. Inspect and record that role before and after apply
and build; include it in the authorized operation and cleanup accounting.

```sh
tofu -chdir=infra/aws/stacks/worker-image apply /absolute/path/image.tfplan
tofu -chdir=infra/aws/stacks/worker-image output image_pipeline_arn
aws imagebuilder start-image-pipeline-execution \
  --region YOUR_REGION --image-pipeline-arn EXACT_PIPELINE_ARN
aws imagebuilder get-image \
  --region YOUR_REGION --image-build-version-arn RETURNED_BUILD_VERSION_ARN
```

Retain the start reply immediately in durable operator evidence. If the reply is
lost, reconcile `list-image-pipeline-images --image-pipeline-arn EXACT_PIPELINE_ARN`
before starting again. A pending run is not a successful AMI. Require `AVAILABLE`
and inspect `outputResources.amis`, the AMI's owner/Region/private launch
permissions, snapshots and provenance tags. Record the component/image definition
digests, resolved parent, exact bundles/manifests and installed package metadata
(`list-image-packages --image-build-version-arn EXACT_BUILD_VERSION_ARN`). Use the
resulting concrete AMI ID as `worker_ami_id` in the deployment root.

## Updating and cleanup

An AMI with the same Helmr bundles can still have a different restore profile.
The host kernel release contributes to the VM platform identity. Before a
preserving update, verify actual source and target kernels and full registered
restore profiles, then follow the [maintenance procedure](../../../../packages/web/src/content/docs/self-hosting/upgrades.md).
A successful image build alone proves neither restore compatibility nor runtime
readiness. A base-image-only update also does not test a Control Plane/schema
upgrade between Helmr releases.

Keep native state and exact build/AMI/snapshot identities outside temporary
worktrees. Image Builder compute and snapshots incur AWS charges. Before cleanup,
ensure no deployment still references the images. Cancel unfinished builds and
confirm their terminal state and temporary compute removal. Delete the exact
build-version records, deregister unused AMIs and delete their unreferenced
snapshots, then inspect and apply the stack's saved destroy plan. Destroying the
pipeline alone does not remove produced AMIs or snapshots. Reconcile state with
AWS inventories, including residual volumes and logs. Treat the account-wide
service-linked role separately: do not delete it while other Image Builder
usage depends on it.
