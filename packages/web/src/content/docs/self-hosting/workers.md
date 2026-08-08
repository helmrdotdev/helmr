---
title: Workers
description: Configure Firecracker worker groups, capacity, networking, enrollment, and safe replacement.
---

# Workers

Workers are optional during Control Plane setup, but task builds and runs need active worker capacity. The AWS compositions create separate logical run and build groups from the same module so their host capacity can be sized independently.

## Evaluation worker

The evaluation profile has workers and NAT disabled by default. For a bounded end-to-end smoke test:

```hcl
enable_nat_gateway                  = true
create_worker                       = true
worker_instance_type                = "c8i.xlarge"
worker_enable_nested_virtualization = true
worker_min_size                     = 1
worker_max_size                     = 1
worker_root_volume_size_gb          = 120
worker_disk_mib                     = null
```

Use only an EC2 family that supports nested virtualization. Keep NAT enabled while a private worker is running or draining.

## Production capacity

The standard profile defaults to a metal worker instance type, nested virtualization off, and zero minimum capacity. Set explicit run and build minimums, maximums, instance types, root-volume performance, VM sizing, cache limits, and execution slots for your workload. `max_size` is the infrastructure spend guardrail; equal minimum and maximum values express fixed capacity.

Workers are filesystem-first. Their root EBS volume holds runtime data and guest artifacts, and build-capable workers use dedicated cache and scratch filesystems on that host storage. Leave `worker_disk_mib = null` to advertise detected filesystem capacity, or set it to cap the advertised value. The worker always withholds `worker_disk_reserve_mib` before certifying usable capacity.

## AMI and enrollment contract

The official AMI is selected from the release manifest by `helmr_version` and `aws_region`. A custom AMI must contain the worker binary and unit, Firecracker, jailer, `ip`, `nft`, AWS CLI v2, curl, KVM support, and certified guest boot artifacts under the configured images directory.

At boot, the module fetches the worker-group enrollment token into a root-only volatile file. The token selects the logical group. AWS identity, AMI provenance, instance profile, Auto Scaling membership, and fleet policy remain infrastructure responsibilities; the Control Plane does not authenticate or allowlist the AMI.

Workers need outbound access to the Control Plane, S3, ECR, AWS APIs, registries, and task dependencies. The deployment-owned blocked-CIDR set must cover the full execution VPC prefix. SSM Session Manager is enabled by default, and no inbound SSH rule is required.

## Drain and replace

New instances start protected from scale-in. Launch-template changes do not automatically refresh instances. Before provider deletion or an AMI rollout, drain the exact logical worker until it reaches `termination_ready`, then explicitly coordinate the Auto Scaling instance refresh.

For a manual diagnostic drain:

```sh
helmr-worker drain --timeout 30m
```

Do not reduce desired capacity or terminate a host first: provider scaling must not bypass the claim-fenced drain path. Check connectivity and activation with:

```sh
helmr-worker status
```

The status command exits non-zero unless the worker can authenticate to the Control Plane and is active.
