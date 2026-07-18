# Helmr AWS Worker Image Module

This module creates an EC2 Image Builder pipeline for the worker AMI consumed by
`modules/worker`.

The image build clones the configured Helmr repository/ref, enters the Nix `smoke-linux` shell, and
installs:

- `helmr-worker`, `helmr`, `helmr-control`, and `helmr-dispatcher`
- Firecracker and jailer
- BuildKit and OCI runtime tooling
- CNI plugins including `tc-redirect-tap`
- guest boot artifacts under `/var/lib/helmr/images/guest/out`
- the authenticated runtime release corpus under `/usr/lib/helmr/runtime-release`
- `helmr-buildkit.service` and `helmr-worker.service`

Every image recipe requires a version-pinned, uncompressed runtime release tar
through `release_package_s3_uri`, `release_package_object_arn`,
`release_package_version_id`, and `release_package_sha256`. The package contains
the global catalog, Sigstore bundle, trusted roots, and the `x86_64` verifier
corpus. The SHA-256 input is the 64-character lowercase hex value without a
`sha256:` prefix. `release_package_kms_key_arn` is required only when that
staging object uses a customer-managed KMS key. This transport is separate from
the optional Git source bundle. When `instance_profile_name` selects a
caller-owned profile, its role must carry the equivalent version-constrained
`s3:GetObjectVersion` and optional `kms:Decrypt` permissions.

Run the emitted `image_pipeline_arn` with EC2 Image Builder, then pass the produced AMI ID to the
worker module as `worker_ami_id`.

By default the module distributes a private AMI in the provider region and encrypts the root volume
snapshot. For official customer releases, set `distribution_regions` to the supported regions,
`ami_public=true`, and `root_volume_encrypted=false`. AWS public AMIs cannot use encrypted
snapshots.
