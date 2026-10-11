---
title: Upgrades and capacity maintenance
description: Maintain fixed AWS worker capacity through whole-population drain, source removal, and explicit activation.
---

# Upgrades and capacity maintenance

The AWS Standard and Quickstart references use explicit fixed capacity. Set
`worker_count` when `create_worker=true`; desired and maximum capacity equal that
count, with minimum zero. Increasing capacity uses the same qualified build and
restore profile. Every worker update and every reduction, including reduction
to zero, follows the full-stop procedure below. Expect an interruption until the
configured replacement count is ready. There is no automatic instance refresh,
capacity controller, maintenance timeout, or automatic rollback.

Use a release pair qualified for this procedure: the same worker API revision
and compatible restore profiles for every resident and parked dependency. These
instructions describe the required gates; a local Terraform test does not qualify
a release pair or prove a real AWS update. Require installation and maintenance
evidence for the exact released artifacts before using them for production.

## Prepare artifacts, recovery and authentication

Record the exact source and target `helmr_version`, digest-pinned
`controlplane_image`, `worker_ami_id`, signed release index, worker runtime/rootfs
profile, database schema, Terraform state and recovery point. Retain the storage,
checkpoint and root encryption keys. Rehearse restoring data separately; an image
rollback cannot undo a database migration. A custom AMI ID is a locator, not a
Control Plane attestation of its contents. Follow the
[release artifact requirements](/docs/self-hosting/requirements#release-artifacts):
common releases provide a signed index and host/runtime bundles, not an AWS AMI
or `aws-artifacts.json`. Prepare the AMI from those verified bytes and supply
explicit image/AMI inputs. Do not rebuild different binaries and call that a
public-artifact installation.

Before the operators' first verified-email login, configure their `controlplane_environment.ADMIN_EMAILS`
in the reference root. This bootstraps ordinary deployment administrator authority
for the separate Pool retirement operations below. It does not promote existing
users or change existing grants; an organization owner is not automatically a
deployment administrator. Keep this login session separate from the Capacity token.

Use the deployment [Capacity API](/docs/self-hosting/capacity-scaling) credential
through an authorized operator session. In Standard or Quickstart, configure
`capacity_token_secret_arn` with a Secrets Manager secret containing the token;
set `capacity_token_kms_key_arn` as well when that secret uses a customer KMS key.
The Control Plane receives the value at startup. Terraform receives only ARNs.
Keep shell tracing and HTTP debug output disabled. Never put the token in a
command argument, file, Terraform variable, or log. In a shell with command
substitution, load the already-configured secret into a non-exported variable:

```sh
set +x
unset CAPACITY_TOKEN
CAPACITY_TOKEN="$(aws secretsmanager get-secret-value \
  --secret-id "$CAPACITY_TOKEN_SECRET_ARN" --query SecretString --output text)"
```

Use HTTPS and pass authentication through curl's header input, as in the examples
below. Unset `CAPACITY_TOKEN` when the session ends. The examples assume the AWS
CLI's already-authorized account and Region and require checking every command's
exit status before continuing.

Read `GET /capacity/v1/deployment` and resolve the Worker Group and each Pool.
The deployment read reports one Control Plane replica's release, source commit,
worker API revision and database schema/dirty flag; verify all service replicas
converged separately. Group/Pool resolution reports `retained_profiles`, including
live Instances and creating or parked checkpoints. Require `complete: true` and
verify target profile coverage. An incomplete report, incompatible profile,
dirty migration or unknown artifact blocks the update.

Export `tofu output -json worker_generation_definitions` and
`tofu output -json worker_generation_bindings`. Keep the exact Pool-to-ASG,
launch-template version and artifact bindings with your maintenance record.
For a worker update, copy the source definition **unchanged**, including its
`count`, into `retained_worker_generations`. Set the new generation's
`worker_count = 0`. Its immutable identity must differ from the source.

Review a native `tofu plan` and apply only inert preparation: the source ASG,
count, launch template, IAM and user data must remain unchanged; the new target
must have zero desired/maximum capacity and no automatic refresh. Pin the old
Control Plane and dispatcher images explicitly while preparing a different
`helmr_version`; preparation must not update the services or schema. Do not apply
a plan that removes a source, changes its sealed identity, truncates its capacity,
or replaces shared infrastructure needed by running work.

For a same-build count reduction, keep the existing generation and its unchanged
count until the drain gate. Do not apply the lower count yet.

## Inhibit launches and drain the whole population

Stop other capacity writers. For every source ASG, record the original suspended
processes, then inhibit launch and refresh:

```sh
aws autoscaling describe-auto-scaling-groups \
  --auto-scaling-group-names "$SOURCE_ASG" --output json
aws autoscaling suspend-processes \
  --auto-scaling-group-name "$SOURCE_ASG" \
  --scaling-processes Launch InstanceRefresh
```

Inspect ASG instance membership, scaling activities, pending lifecycle actions,
and EC2 instance state. Account for launches already in flight; suspending Launch
does not undo them. Terraform preserves native `suspended_processes` changes
across subsequent applies. Wait until the population is stable with no pending launch.
Do not start or resume an instance refresh. AWS scale-in protection does not
prevent health replacement or manual termination; any population change requires
fresh reconciliation. See [AWS process suspension](https://docs.aws.amazon.com/autoscaling/ec2/userguide/as-suspend-resume-processes.html).

For every exact EC2 instance ID in every source group, query
`GET /capacity/v1/worker-hosts?worker_group_id=...&resource_id=...&limit=500`.
Match its Group and Pool to the recorded provider binding. This API is bounded,
not a paginated complete fleet inventory: an unfiltered list or a response at the
limit cannot prove completeness. Missing, duplicate or ambiguous matching records
block maintenance. Also reconcile Product Hosts with unreclaimed Instance state
against provider inventory so absent hosts are not silently omitted.

Record each canonical Host `id`, `current_epoch` and `claim_version`. Submit drain
for **all** source Hosts as one whole-population phase, before deleting any source.
For each matched Host, use its fresh fences:

```sh
printf 'Authorization: Bearer %s\n' "$CAPACITY_TOKEN" |
  curl --fail-with-body --request POST --header @- \
    --header 'Accept: application/json' --header 'Content-Type: application/json' \
    --data "{\"expected_epoch\":$CURRENT_EPOCH,\"expected_claim_version\":$CLAIM_VERSION,\"reason\":\"replacement\"}" \
    "$CONTROL_PLANE_URL/capacity/v1/worker-hosts/$HOST_ID/drain"
```

Use `capacity_reduction` for a count reduction. On `409`, re-read the Host and
reconcile its identity and epoch; never retry with guessed fences. A partially
submitted batch is not a completed drain phase. Keep Control Plane, dispatcher,
worker connectivity, renewal, capture and cleanup running throughout the wait.
You may pause submissions upstream; otherwise new work can queue durably.

Do not substitute Group drain, Pool drain/retirement or on-host `worker drain`.
They do not implement this full-stop procedure. A long-running Turn may block indefinitely.
A timeout never permits deleting it or inventing loss. Wait, or have its owner
explicitly cancel named work through its normal API. Maintenance does not choose
cancellation, discard parked state or replay a Turn.

## Verify one all-source gate

Re-read the provider population and each exact Host. Proceed only when all of
these hold together:

- Every source ASG is launch-inhibited, its population is unchanged, and no launch
  or refresh is in flight.
- Every present non-lost Host is at the recorded epoch and `termination_ready`.
  Its `drain_blockers` are zero and local cleanup has been acknowledged. Zero
  blocker counts alone are insufficient.
- Every actually absent Host either already reached `termination_ready` at the
  recorded epoch, or has a recorded loss disposition and reconciled Product loss
  recovery. A ready Host that subsequently disappeared needs no `/lost` call;
  that operation does not accept `termination_ready`. Report loss and its retry or state-loss outcome as loss,
  never as graceful drain.
- Re-read retained profiles and dependencies; they remain complete and covered by
  the target or retained restoration authority.

For confirmed provider absence without an existing `termination_ready` record, call `POST /capacity/v1/worker-hosts/{id}/lost`
with the same header-input authentication and no request body. Confirm absence
from both ASG membership and exact EC2 state first. Do not use this operation to
turn an active or draining present Host into an acceptable loss.

There is one pre-gate deletion exception: Product already reports the exact
Host/epoch as `lost`, but its EC2 instance is still present. After verifying that
identity and launch inhibition, remove only that instance and decrement desired
capacity once:

```sh
aws autoscaling terminate-instance-in-auto-scaling-group \
  --instance-id "$LOST_EC2_INSTANCE_ID" --should-decrement-desired-capacity
```

Wait for actual termination, re-read the whole population and Product loss
recovery, and account for the decremented desired count. Do not reapply the old
count while reconciling this exception. Do not blindly retry the decrement after
an interrupted response. It is not permission to lower desired/max for healthy
hosts. See [AWS exact-instance termination](https://docs.aws.amazon.com/cli/latest/reference/autoscaling/terminate-instance-in-auto-scaling-group.html).

## Empty the source, then activate

Only after the complete gate, clear scale-in protection on the observed ready
instances, in batches of at most 50 per ASG:

```sh
aws autoscaling set-instance-protection \
  --auto-scaling-group-name "$SOURCE_ASG" \
  --instance-ids "$READY_EC2_INSTANCE_ID" --no-protected-from-scale-in
```

Re-observe the gate immediately before applying capacity changes. Set the source
`count` to zero in `retained_worker_generations`, or set `worker_count = 0` for a
same-generation reduction. Review and apply the plan while launch inhibition
remains in place. Confirm every source ASG is empty and its EC2 instances are
terminated. Do not remove the retained definition just because its ASG is empty:
restore authority and recovery may still need its artifacts, IAM and keys.

Follow the target release's qualified schema/Control Plane/dispatcher ordering.
Run migrations from the exact target image and require exit zero before deploying
that image to services. Verify `/healthz` and `/readyz` and the live deployment
metadata. Database rollback is a separate recovery procedure.

Configure the explicit positive target `worker_count`, review and apply its plan.
The ASG provider capacity waiter is disabled so this configuration apply does not
wait for hosts while launches are suspended. Apply success is not readiness.
For reuse of the same ASG, configure this count only after the source is empty,
then resume only the processes this maintenance session suspended. For example,
if neither was suspended beforehand:

```sh
aws autoscaling resume-processes \
  --auto-scaling-group-name "$SOURCE_ASG" \
  --scaling-processes Launch InstanceRefresh
```

For a new inert target, confirm its launch settings permit the qualified activation.
Require the configured count of fresh active, unpaused Hosts at their current
epochs in the exact target Pool. Then select the Pool with the fresh Group claim:

```sh
printf 'Authorization: Bearer %s\n' "$CAPACITY_TOKEN" |
  curl --fail-with-body --request PUT --header @- \
    --header 'Accept: application/json' --header 'Content-Type: application/json' \
    --data "{\"pool_id\":\"$TARGET_POOL_ID\",\"expected_group_claim_version\":$GROUP_CLAIM_VERSION,\"minimum_ready_hosts\":$TARGET_COUNT}" \
    "$CONTROL_PLANE_URL/capacity/v1/worker-groups/$GROUP_ID/primary-pool"
```

A stale claim requires re-observation. Selecting the already-current primary is
an exact replay and does not check current readiness, so always verify ready
capacity independently. A same-generation reduction may retain its current
primary. Verify restored work and fresh work before declaring service restored.

At zero, keep launch inhibition and explicitly record intentional no-capacity
configuration. This is not service readiness. Selecting a release at zero is
configuration only; its first positive activation still needs qualification,
readiness and primary selection. Pending work remains subject to normal deadlines,
cancellation and retention while it waits.

## Retire an old Pool separately

Retirement withdraws restoration authority; it is not part of routine host drain.
Keep an old generation while recovery or retained execution needs its profile.
A zero count does not authorize removing its definition, artifacts, IAM or keys.

Using an authenticated deployment administrator session, resolve the old Pool and
its current claim, then call
`POST /admin/api/v1/worker-groups/{groupID}/pools/{poolID}/drain`
with `{"expected_pool_claim_version": <current claim>}`. This withdraws an active
Pool from new enrollment and restoration eligibility. Product rejects retirement
of the current primary or removal of a required last restoration path. A pending,
never-activated Pool can proceed directly to the disable check.

Keep launches inhibited. Confirm no pending launch and exact physical absence,
then re-read the Pool's claim and call
`POST /admin/api/v1/worker-groups/{groupID}/pools/{poolID}/disable`
with the new `expected_pool_claim_version`. Preserve the successful response with
`status: "disabled"` for that exact Pool. On a conflict, re-observe and preserve
the generation; do not bypass the predicate. The Capacity token does not authorize
these administrator operations.

The disabled response is irreversible Product authorization for cleanup, not
proof that AWS is empty. Recheck provider inventory and pending launches again
before removing the matching `retained_worker_generations` entry and reviewing
its deletion plan. A disabled identity cannot be revived by reapply or rollback;
use a new qualified generation for future capacity. Never delete shared artifacts
or keys still referenced by another retained generation or recovery point.

## Interrupted sessions and recovery

On reconnect, re-read live artifacts, Group/Pool/Host claims, provider membership,
launch settings and the complete gate. A saved success flag never authorizes a
later mutation. Before the first drain, you may abandon preparation by restoring
the unchanged source's original launch settings after re-observation.

Once drain begins, those Host epochs cannot be reopened. Resuming service before
they finish requires separately qualified same-release capacity in a fresh
Pool/provider binding, with an explicit operator-approved capacity and budget
change. Retain the unchanged source definition and use a new
`worker_generation_key` while keeping the same qualified AMI and restore shape;
prepare that recovery generation at zero first. Preserve the original draining owners and keep their source inhibited
until empty. Fresh recovery capacity needs ready-count verification and primary
selection too. Without a qualified recovery procedure, remain blocked.

If the target fails after source removal, work may remain queued. Use the tested
target or recovery-point procedure, preserving data and keys. Retain required
artifacts and account for infrastructure cost until recovery and retirement are
complete; neither rollback nor incident response is automated.

## Worker API version changes

Enrollment, credential refresh and activation check the worker API revision;
ordinary authenticated calls do not prove compatibility after a Control Plane
replacement. Incompatible API or restore-profile transitions are blocked until a
separately qualified release procedure exists or dependent work finishes normally.
There is no generic checkpoint converter or guarantee that an older image can
resume newer state. Do not improvise an in-place downgrade.
