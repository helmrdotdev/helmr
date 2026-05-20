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
- `buildkit.service` and `helmr-worker.service`

Run the emitted `image_pipeline_arn` with EC2 Image Builder, then pass the produced AMI ID to the
worker module as `worker_ami_id`.

By default the module distributes a private AMI in the provider region and encrypts the root volume
snapshot. For official customer releases, set `distribution_regions` to the supported regions,
`ami_public=true`, and `root_volume_encrypted=false`. AWS public AMIs cannot use encrypted
snapshots.
