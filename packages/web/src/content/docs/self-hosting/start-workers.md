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
- CNI configuration and plugins, including `tc-redirect-tap`.
- BuildKit service.
- AWS CLI v2 and curl.
- Guest kernel, initramfs, and rootfs artifacts.
- SSM agent for maintenance.

Workers are filesystem-first Firecracker hosts. Size the root EBS volume for build cache, runtime
state, and guest artifacts. Leave `worker_disk_mib` null for auto-detected filesystem capacity, or
set it to cap the capacity workers advertise.

Both deployment modes create separate run and build worker groups from the same worker module. Each group has independent instance type, minimum, warm, and maximum capacity, so a self-hosted deployment can run at minimum cost or scale without changing the internal architecture.

Every AWS worker enrolls with a fresh one-time nonce and an IMDS-credential-signed STS request.
Control verifies the account, region, instance profile, Auto Scaling group, AMI, resource identity,
and worker group before issuing a scoped worker credential. Self-hosted AWS resources stay in the
customer account; only the setup and billing surface differs from managed cloud. Certification,
activation, supervisor, scaling, drain, build, and run paths are identical.

Workers outside the AWS topology are not accepted by this release. Additional providers require a
provider identity verifier and capacity actuator with the same enrollment and lifecycle contract;
they do not reuse a long-lived bootstrap token.

Before terminating or replacing worker instances, drain them:

```sh
helmr-worker drain --timeout 30m
```
