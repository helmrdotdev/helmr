---
title: Start workers
description: Enable worker capacity for Firecracker-backed run execution.
section: Self-hosting
sidebarLabel: Start workers
order: 770
---

# Start workers

You can operate the control plane without workers, but runs require at least one active worker.

The `quickstart` profile does not run code by default. For a quick end-to-end smoke test from that profile, enable NAT and one worker:

```hcl
enable_nat_gateway = true
create_worker = true
worker_min_size = 1
worker_max_size = 1
worker_instance_type = "c8i.xlarge"
worker_enable_nested_virtualization = true
worker_root_volume_size_gb = 100
worker_disk_mib = null
```

For production, start from the `standard` profile and size worker capacity for expected concurrency:

```hcl
create_worker = true
worker_min_size = 1
worker_max_size = 3
build_worker_min_size = 1
build_worker_max_size = 2
```

Official worker AMIs are resolved from the Helmr release artifact manifest for the selected `helmr_version`. If you use a custom AMI, it must include:

- `helmr-worker` binary.
- Firecracker and jailer.
- `ip` and `nft` for the Worker-owned routed-TAP datapath.
- AWS CLI v2 and curl.
- Certified guest kernel, initramfs, and rootfs artifacts; the rootfs contains the pinned BuildKit daemon used only inside fresh image-build guests.
- SSM agent for maintenance.

Workers are filesystem-first Firecracker hosts. Size the root EBS volume for build cache, runtime
state, and guest artifacts. Leave `worker_disk_mib` null for auto-detected filesystem capacity, or
set it to cap the capacity workers advertise.

The AWS deployment creates separate logical run and build worker groups from
the same worker module. Each group may use different host capacity and role
settings. Physical host counts and provider scaling policy remain operator
responsibilities.

Every worker enrolls with a fresh one-time nonce and a proof derived from its
group-specific deployment secret. The AWS module fetches that secret during
boot into a root-only volatile file and reports the EC2 instance ID only as an
opaque resource locator for operator correlation. Control verifies the logical
group proof and requested role; it does not verify AWS account, Region, AMI,
instance profile, or Auto Scaling group membership.

Before terminating or replacing worker instances, drain them:

```sh
helmr-worker drain --timeout 30m
```
