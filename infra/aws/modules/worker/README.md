# Helmr AWS Worker Module

This module provisions EC2 Auto Scaling capacity for Linux Firecracker workers. Workers are
filesystem-first hosts: runtime caches, VM state, and guest artifacts live on the instance root
volume. The module does not build the worker AMI.

## Worker AMI Contract

The AMI must provide:

- `worker` at `worker_binary_path`
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
starts `worker` and a small lifecycle watcher. Every execution Worker allocates
and mounts fixed runtime-cache and VM-arena ext4 filesystems. Deployment builds run in the
user-owned local or CI builder, never on a managed Worker.

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
runtime/cache load. Leave `worker_disk_mib` null to let `worker` detect local
filesystem capacity, or set it when the capacity advertised to the Control Plane should be capped.
`worker_disk_reserve_mib` is always passed explicitly (default `1024`) and is withheld before
workload, scratch, and cache partitions are certified.

SSM Session Manager access is enabled by default through `AmazonSSMManagedInstanceCore`, avoiding
inbound SSH rules for bootstrap and smoke debugging. Set `enable_ssm = false` only if the AMI role is
managed elsewhere.

Each fleet is one immutable execution Pool generation. The required
`worker_pool_name` identifies this exact immutable supply generation.
The caller must allocate a new canonical Pool name before changing the AMI or
another sealed runtime/capacity input. During boot, the module fetches the enrollment token into a
root-only volatile file. The token selects the Worker Group, the Pool name
binds the instance to one logical generation, and the EC2 instance ID remains
an opaque operator locator. AWS identity and fleet configuration remain
infrastructure responsibilities.

## Lifecycle

One deployment owner controls capacity. The Standard and Quickstart roots set
`desired_capacity` and `max_size` to the explicit count and `min_size` to zero. A custom
external capacity owner may leave `desired_capacity = null`; the module still
enforces its min/max guardrails. New instances start protected from scale-in.
Protection does not stop health replacement or manual termination and is not a
substitute for the Product drain gate. Native maintenance owns
`suspended_processes`; Terraform ignores changes to that field so applying a
count change does not silently lift launch/refresh inhibition. The provider capacity
waiter is disabled: a count apply can complete with launches suspended, and the
operator separately verifies provider convergence and Product readiness.

When capacity is raised, the launch lifecycle hook keeps the instance out of service until the
worker systemd unit is active. Planned removal first drains the exact host to
`termination_ready`; `worker drain --wait-timeout` bounds observation only and
leaves admitted work running when that wait expires. The termination lifecycle
hook handles provider termination that has already begun. It verifies the local
instance identity and termination state before bounded cleanup, then fences actual
loss if cleanup cannot finish. That hook is not authority to force a planned drain.

Launch-template changes do not start an automatic instance refresh. Worker
updates require a new immutable Pool/provider generation; preserve the old
sealed definition and prepare the new generation with zero capacity. The
[reference maintenance procedure](../../../../packages/web/src/content/docs/self-hosting/upgrades.md)
drains the entire source population, verifies the gate, empties the source,
and then activates explicit target capacity. Do not start an instance refresh
as a shortcut. Control Plane does not maintain an AMI allowlist.

## Permissions boundary and retained authority

`permissions_boundary_arn` optionally selects a caller-owned customer-managed IAM
policy in this account and partition. The module reads that policy and attaches
it without creating or modifying a managed policy. Its inline resource grants
and optional SSM attachment remain unchanged. With a null input, the module
creates its existing least-privilege boundary, including the SSM ceiling.

Persist the emitted `sealed_provider_definition` with each retained generation.
It includes the explicit nullable `boundary_policy_arn` (null for a generated
boundary) and the exact policy document. A retained external ARN is authoritative;
leave the input unset or supply that same ARN. A changed ARN or canonically
changed external policy document fails planning. Do not substitute today's
boundary for a retained generation or omit the nullable discriminator. The
quickstart and standard roots round-trip this current sealed record.


Computer storage requires explicit `computer_save_interval_seconds` and
`computer_devices`. The interval schedules background saves; it is not an RPO or
mandatory Turn-completion barrier. The device list is an exclusive NBD allowlist.
Temporary API failures before save admission retry the same operation while its
writer authority remains valid. Each request is bounded, and writer expiry or
quiescing stops the retry. A continuing failure logs its elapsed time and attempt
count once per minute; successful writer renewal can keep that retry alive.
Capture and post-capture settlement failures retain
their existing preservation-failure handling.
Bootstrap loads NBD with enough device indices, persists that module configuration
across reboot, and rejects connected devices before starting the Worker. Supply
sufficient devices for concurrent Computers and preparation; they are not shared
with another service. The Worker image must contain `kmod` and the host NBD module.
