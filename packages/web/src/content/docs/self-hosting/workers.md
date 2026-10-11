---
title: Workers
description: Configure Firecracker worker groups, capacity, networking, enrollment, and safe replacement.
---

# Workers

Workers are optional during Control Plane setup and are used only to execute
verified Deployment bundles. The AWS compositions create immutable execution
Pool generations so old restore-compatible capacity can remain available at
scale zero during a rollout.

## Control Plane outages and host loss

The Dispatcher requires `CONTROL_PLANE_URL` to be the same public API endpoint
used by Workers. The AWS compositions and dedicated development profile set this
automatically. It must remain reachable from the Dispatcher, including through
CloudFront when enabled.

The Dispatcher suspends observation-age fencing when that endpoint's `/readyz`
check fails. Readiness includes database connectivity, schema readiness and a
check that the Control Plane database session is not read only. After an outage,
a missed check or a Dispatcher restart, readiness checks must succeed for 120
seconds before stale-host fencing resumes. Live Workers can report again during
that window; Hosts that remain stale are fenced under their current epoch and
claim. Logs report fencing suspension and recovery.

This protects against shared API availability failures visible from the
Dispatcher. It does not detect a failure confined to Worker authentication or the
observation handler, inconsistent credentials between API replicas, a hidden bad
replica, or a network partition that only affects Workers. Readiness is not proof
that every observation can commit. Existing ownership expiry and actual provider
loss remain separate; an outage does not extend execution leases indefinitely.
All Dispatcher replicas must run this behavior before relying on the protection.

## Evaluation worker

The evaluation profile has workers and NAT disabled by default. For a bounded end-to-end smoke test:

```hcl
enable_nat_gateway                    = true
create_worker                         = true
worker_host_type                      = "c8i.xlarge"
worker_enable_nested_virtualization   = true
worker_count                          = 1
worker_capacity_vcpus                 = 3
worker_capacity_memory_mib            = 6144
worker_execution_slots                = 2
worker_root_volume_size_gb            = 512
worker_root_volume_iops               = 3000
worker_root_volume_throughput         = 125
worker_disk_mib                       = 491520
worker_disk_reserve_mib               = 8192
worker_vm_vcpus                       = 1
worker_vm_memory_mib                  = 2048
worker_vm_scratch_disk_mib            = 32768
worker_computer_staging_mib           = 65536
worker_computer_save_interval_seconds = 60
worker_computer_devices               = ["/dev/nbd0", "/dev/nbd1", "/dev/nbd2", "/dev/nbd3"]
```

This profile was qualified with sequential smoke workloads; concurrent capture
and restore on both slots remain a separate runtime qualification. The explicit
480-GiB disk ceiling leaves 456 GiB after the 8-GiB reserve and 16-GiB cache.
Each slot now needs just over 216 GiB for its full lifecycle: scratch, Computer
projection and staging, runtime and program files, retained restored memory/state,
and simultaneous checkpoint intermediates. Both slots fit the configured supply.
The Worker also checks fresh available filesystem space before activation; existing
files can make an otherwise sufficient configured disk fail startup. Keep the
ceiling below actual filesystem capacity and fund every configured slot.

Use only an EC2 family that supports nested virtualization. Keep NAT enabled while a private worker is running or draining.

## Production capacity

The standard profile defaults to a metal worker instance type and nested virtualization off. Set an explicit `worker_count` when enabling Workers, along with the instance type, root-volume performance, VM sizing, cache limits, and execution slots for your workload. The reference roots set desired and maximum capacity to that count and minimum capacity to zero.

Workers are filesystem-first. Their root EBS volume holds runtime data, staged
artifacts, and cache. The AWS reference roots require explicit CPU, memory,
execution-slot, cache and disk capacity when Workers are enabled, including a
non-null `worker_disk_mib`. The Worker subtracts its reserve and cache from the
configured ceiling, then requires a complete lifecycle envelope for every slot.
After local recovery it checks fresh filesystem availability with the same reserve,
cache and original slot count, including quarantined owners. Unaccounted temporary
files reduce that availability. A shortfall prevents activation and reports the
required and available bytes and the affected work and temporary directories.
Stop the Worker and establish file ownership before manually removing residue;
unknown temporary files are preserved automatically. Alternatively, provision
enough capacity for the retained files and the configured slots.

The AWS module rejects a `worker_disk_mib` ceiling larger than
`root_volume_size_gb * 1024`. That nominal volume capacity is not all available
space: filesystem metadata, reserved blocks and image contents reduce fresh
availability. Leave headroom for these costs when sizing the volume.

## Computer storage

The host must provide an exclusive NBD device pool. Set
`WORKER_COMPUTER_DEVICES` to a space-separated allowlist of devices dedicated to
this Worker (for example, `/dev/nbd0 /dev/nbd1`). Do not share these devices with
another service. The Worker fails startup without an explicit allowlist.

`WORKER_COMPUTER_STAGING_MIB` bounds local encrypted disk version staging per
Instance (default: 65536 MiB). It is additional to guest scratch and the Computer
projection, and does not change the Computer's logical disk size or make local
writes externally durable. The AWS roots expose `worker_computer_staging_mib`;
the Worker module exposes `computer_staging_mib`. The value is part of each
immutable generation, including retained generations.

Host disk supply and advertised guest disk are distinct. Guest supply is the
configured slot count multiplied by per-VM scratch; physical disk also funds
Program/runtime files, restore inputs and later capture. Incoming checkpoint
artifacts must fit the admitted VM shape before materialization. AWS planning
rejects explicit disk configurations that cannot fund these files. Genuine
preparation failures retain their existing bounded retry and exhaustion behavior.

`WORKER_COMPUTER_SAVE_EVERY` is required and must be a positive Go duration.
It controls background disk preservation while an execution is running. Choose
it using the workload's write rate, staging capacity, and acceptable loss window;
there is no built-in default. An in-flight save coalesces ticks. This is not a
maximum recovery-point age: upload latency and failures can extend that age.
Successful Turn completion requires its own fresh disk cut and durable publication;
it does not wait for the periodic interval. Required saves take priority over new
background saves, while an already admitted save finishes first. Any successful
publication restarts the interval. Managed waiting and `idleTimeout` govern
execution suspension separately. Successful adoption allows bounded local staging
cleanup. Published roots remain retained while the writer lease is live, so both
write volume and lease duration affect retained storage. Save records and object
pins also accumulate each interval while the lease is live, even when the resulting
disk content is unchanged. A failed live capture
stops the allocation and interrupts its running work; an uncertain publication
retains its original cut for reconciliation.

The Worker binary runs its own NBD helper. Keep the Computer preparation arena
and VMM state on the same filesystem. If the Worker dies while a helper owns a
device, retain the arena and reconcile the exact owner before reusing the device;
missing process-local state is not proof that the attachment was released.
Startup and drain completion refuse the whole host while a direct
`WORKER_WORK_DIR/tmp/computer-*` arena retains `config.json`, `claim.json` or
`nbd.sock`, including a released claim journal, or while any configured NBD
device cannot be verified idle and exclusively opened. Unreadable or symlinked
temporary roots and Computer arenas also block recovery. Before removing an
exact retained arena, prove that its consumer and descendants have stopped,
resolve its Saves' publication or loss outcomes, release its known helper and
device, and verify device inactivity. Preserve ordinary temporary data; a
subsequent Worker startup checks custody again before reporting recovery.

If network cleanup cannot prove that retained VM resources are gone, the affected
Worker cannot accept new work. Startup logs identify quarantined owners and their
cleanup errors, including when runtime qualification fails before the Worker can
report to the Control Plane. Each ordinary startup retries cleanup of resources
whose exact identity still matches the retained manifest. Keep owner markers,
network manifests and retained resource reservations until recovery completes.

For persistent failures, stop the Worker and establish the actual ownership and
authority to reconcile each affected resource. A manifest naming a link does not
authorize deleting a replacement or foreign link. After authorized reconciliation,
restart the Worker so ordinary recovery, runtime qualification and Control Plane
activation recheck the result. Do not bypass overlap checks or activation fences.

## AMI and enrollment contract

Prepare `worker_ami_id` from the verified public release's host/runtime bundles as described in [release artifact requirements](/docs/self-hosting/requirements#release-artifacts). Common releases do not publish AWS AMIs. Use the public checkout's [`infra/aws/stacks/worker-image/README.md`](https://github.com/helmrdotdev/helmr/blob/main/infra/aws/stacks/worker-image/README.md) for the complete input, build and cleanup procedure; use the documentation from your selected source commit when reproducing an older release. The prepared AMI must contain the worker binary and unit, Firecracker, jailer, `ip`, `nft`, AWS CLI v2, curl, KVM support, and certified guest boot artifacts under the configured images directory.

At boot, the module fetches the worker-group enrollment token into a root-only volatile file. The token selects the logical group. AWS identity, AMI provenance, instance profile, Auto Scaling membership, and fleet policy remain infrastructure responsibilities; the Control Plane does not authenticate or allowlist the AMI.

Workers send their worker API version in the JSON `api_version` field when enrolling, exchanging their host secret for a host credential, and activating. The Control Plane rejects a missing or different revision with HTTP 409 `worker_api_version_mismatch` before the operation changes host state. The error names both API versions. A rejected worker keeps its stored host secret; use a compatible worker release or roll the Control Plane back.

Ordinary worker API calls do not require a version header. Compatible releases keep the same API version. Connection checks do not guarantee compatibility after a Control Plane replacement: drain workers before deploying an incompatible release, as described in [Upgrades](/docs/self-hosting/upgrades#worker-api-version-changes).

Workers need outbound access to the Control Plane, S3, AWS APIs, and task
destinations. They do not install dependencies or build Deployment artifacts.
The deployment-owned blocked-CIDR set must include the exact execution VPC
prefix. SSM Session Manager is enabled by default, and no inbound SSH rule is required.

## Drain and replace

Set an explicit `worker_count` when enabling workers. The reference stacks set
ASG desired and maximum capacity to this count, with minimum zero; there is no automatic
instance refresh. New hosts start protected from scale-in, but protection does
not prevent health replacement or manual termination.

Every update and count reduction uses the [full-stop maintenance procedure](/docs/self-hosting/upgrades).
Drain the whole source population with the deployment Capacity API, wait for all
exact Host epochs to become `termination_ready`, and only then remove that
population. Keep Control Plane, dispatcher and networking available during drain.
Group drain, Pool retirement and on-host `worker drain` have different purposes
and do not implement this procedure. A long-running Turn can postpone maintenance
indefinitely; an observation timeout does not authorize cancellation or deletion.

Check connectivity and activation with:

```sh
worker status
```

The status command exits non-zero unless the worker can authenticate to the Control Plane and is active.
