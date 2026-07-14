# Helmr AWS Worker Module

This module provisions EC2 Auto Scaling capacity for Linux Firecracker workers. Workers are
filesystem-first hosts: build cache, runtime state, and guest artifacts live on the instance root
volume. The module does not build the worker AMI.

## Worker AMI Contract

The AMI must provide:

- `helmr-worker` at `worker_binary_path`
- systemd units named by `worker_service_name` and `buildkit_service_name`
- AWS CLI v2 and `curl`
- Firecracker and jailer binaries
- `/dev/kvm` capable instance support
- CNI config and plugins, including `tc-redirect-tap`
- BuildKit daemon listening on `HELMR_WORKER_BUILDKIT_ADDR`
- guest boot artifacts under `HELMR_WORKER_IMAGES_DIR`

For cost-controlled smoke environments, set `enable_nested_virtualization = true` and use an AWS
instance family that supports EC2 nested virtualization, such as C8i/M8i/R8i. Leave it disabled for
metal worker instances and for instance families that do not support the option.

The module writes `/etc/helmr/worker.env` from Terraform inputs and Secrets Manager values, then
starts BuildKit, `helmr-worker`, and a small lifecycle watcher.

`worker_environment` is only for additional non-secret worker variables. It cannot override
infra-owned `HELMR_*` routing, storage, enrollment, Firecracker, BuildKit, or network policy
settings; use the module inputs for those values.

Size `root_volume_size_gb`, `root_volume_iops`, and `root_volume_throughput` for expected
build/cache/runtime load. Leave `worker_disk_mib` null to let `helmr-worker` detect local
filesystem capacity, or set it when the capacity advertised to the control plane should be capped.
`worker_disk_reserve_mib` is always passed explicitly (default `1024`) and is withheld before
workload, scratch, and cache partitions are certified.

SSM Session Manager access is enabled by default through `AmazonSSMManagedInstanceCore`, avoiding
inbound SSH rules for bootstrap and smoke debugging. Set `enable_ssm = false` only if the AMI role is
managed elsewhere.

`worker_group_id` and `worker_roles` select the enrollment and scheduling boundary in every
deployment. Every AWS worker proves its EC2 identity with temporary instance-profile credentials;
control verifies its account, region, Auto Scaling group, instance profile, and AMI before issuing
the same scoped worker credential used by the runtime path.

## Lifecycle

The application controller is the only desired-capacity writer. Terraform enforces `min_size` and
`max_size`, and new instances start protected from scale in so provider policies cannot bypass
drain selection. Fixed capacity is expressed with equal minimum and maximum values.

When capacity is raised, the launch lifecycle hook keeps the instance out of service until the
BuildKit and worker systemd units are active. During scale-in or instance refresh, the termination
lifecycle hook gives `helmr-worker drain` time to stop accepting leases and wait for active
executions before the instance terminates.

Launch-template changes do not start an automatic instance refresh. Apply the new worker-group AMI
allowlist and launch policy first, then explicitly start the Auto Scaling instance refresh. Remove
the old AMI from the allowlist only after replacement completes.
