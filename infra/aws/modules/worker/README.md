# Helmr AWS Worker Module

This module provisions EC2 Auto Scaling capacity for Linux Firecracker workers. Workers are
filesystem-first hosts: build cache, runtime state, and guest artifacts live on the instance root
volume. The module does not build the worker AMI.

## Worker AMI Contract

The AMI must provide:

- `helmr-worker` at `worker_binary_path`
- the worker unit named by `worker_service_name`
- AWS CLI v2 and `curl`
- `/usr/local/sbin/helmr-prepare-root`, matching the checked-in
  [`prepare-root.sh`](../worker-image/templates/prepare-root.sh) for this Product version and
  installed with mode `0755`
- an Ubuntu ext4 root partition with `growpart`, `resize2fs`, `blockdev`,
  `findmnt`, `lsblk`, and GNU `readlink`
- Firecracker and jailer binaries
- `/dev/kvm` capable instance support
- `ip` and `nft` for the Worker-owned routed-TAP datapath
- guest boot artifacts under `WORKER_IMAGES_DIR`

For cost-controlled smoke environments, set `enable_nested_virtualization = true` and use an AWS
instance family that supports EC2 nested virtualization, such as C8i/M8i/R8i. Leave it disabled for
metal worker instances and for instance families that do not support the option.

The module writes `/etc/helmr/worker.env` from Terraform inputs and Secrets Manager values, then
starts `helmr-worker` and a small lifecycle watcher. Build-capable workers additionally allocate
and mount fixed Worker-cache and image-build scratch ext4 filesystems; all untrusted BuildKit
execution stays inside the fresh image-build VM.

Before reading secrets or allocating runtime storage, launch user data invokes the AMI-owned root
preparation helper with the configured EBS size. The helper verifies the root device, grows its
partition, and resizes the ext4 filesystem. The preparation is idempotent when the parent Ubuntu
image has already completed the resize. Unsupported root layouts fail before Worker enrollment;
the module does not support XFS, LVM, or an unpartitioned root device. Worker user data is kept
below a 15 KiB internal budget so it retains headroom under the EC2 decoded user-data limit.

`worker_environment` is only for additional non-secret Worker variables. Keys managed by the
module through typed inputs, Secrets Manager, or EC2 metadata are reserved even when a conditional
value is absent from the rendered environment. Conflicts fail during planning. Remove conflicting
entries and use the corresponding typed input where one exists; other values are fixed or derived
by the module.

Size `root_volume_size_gb`, `root_volume_iops`, and `root_volume_throughput` for expected
build/cache/runtime load. Leave `worker_disk_mib` null to let `helmr-worker` detect local
filesystem capacity, or set it when the capacity advertised to the Control Plane should be capped.
`worker_disk_reserve_mib` is always passed explicitly (default `1024`) and is withheld before
workload, scratch, and cache partitions are certified.

SSM Session Manager access is enabled by default through `AmazonSSMManagedInstanceCore`, avoiding
inbound SSH rules for bootstrap and smoke debugging. Set `enable_ssm = false` only if the AMI role is
managed elsewhere.

`worker_roles` advertises the subset of roles this fleet serves, while the
required `worker_pool_name` identifies this exact immutable supply generation.
The caller must allocate a new canonical Pool name before changing the AMI or
another sealed runtime/capacity input; role names are not generation
identifiers. During boot, the module fetches the enrollment token into a
root-only volatile file. The token selects the Worker Group, the Pool name
binds the instance to one logical generation, and the EC2 instance ID remains
an opaque operator locator. AWS identity and fleet configuration remain
infrastructure responsibilities.

## Lifecycle

Deployment infrastructure is the only desired-capacity writer. Terraform enforces `min_size` and
`max_size`, and new instances start protected from scale in so provider policy cannot bypass the
exact drain path. Fixed capacity is expressed with equal minimum and maximum values.

When capacity is raised, the launch lifecycle hook keeps the instance out of service until the
worker systemd unit is active. During scale-in or instance refresh, the termination
lifecycle hook gives `helmr-worker drain` time to stop accepting leases and wait for active
executions before the instance terminates.

Launch-template changes do not start an automatic instance refresh. Drain the
exact logical worker instance to `termination_ready` before provider deletion,
then explicitly start or coordinate the Auto Scaling instance refresh. Control Plane
does not maintain an AMI allowlist.
