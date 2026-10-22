---
title: Custom capacity scaling
description: Connect trusted provider-specific scaling automation to a self-hosted Helmr deployment.
---

# Custom capacity scaling

Build a provider-specific scaler against the Capacity deployment protocol at
`/capacity/v1`. Helmr supplies desired-capacity and Worker lifecycle status.
Your scaler owns provider policy and mutations, including creating and
terminating hosts.

This is a deployment infrastructure protocol for a trusted capacity owner,
including an operator performing [full-stop maintenance](/docs/self-hosting/upgrades). It is not
the tenant Developer REST API. Its credential grants every Capacity operation
and Worker group in the deployment.

## Configure authentication

Generate one canonical 32-byte base64url token:

```sh
CAPACITY_TOKEN="$(openssl rand -base64 32 | tr -d '\n=' | tr '/+' '_-')"
```

Store it in your secret manager. Provide the same value as `CAPACITY_TOKEN` to
Control Plane and as a secret to the scaler. Do not place it in source,
command-line arguments, logs, or ordinary configuration files. When
`CAPACITY_TOKEN` is unset, Control Plane rejects every Capacity request.

Send the token in every request:

```http
Authorization: Bearer <capacity-token>
Accept: application/json
```

Use HTTPS outside `localhost` and `127.0.0.1`.

## Operations

The Control Plane base URL is shown as `$CONTROL_PLANE_URL` below.

| Method and path | Purpose |
| --- | --- |
| `GET /capacity/v1/updates` | Receive capacity invalidation hints as server-sent events. |
| `GET /capacity/v1/deployment` | Read the live deployment release, worker API and database schema. |
| `GET /capacity/v1/worker-groups/resolve?region_id=...&name=...` | Resolve a Worker group by canonical Region ID and name. |
| `GET /capacity/v1/worker-groups/{group_id}/pools/resolve?name=...` | Resolve a pool in a Worker group. |
| `PUT /capacity/v1/worker-groups/{group_id}/primary-pool` | Select the primary pool with a group claim-version fence and a positive minimum of ready hosts. |
| `POST /capacity/v1/worker-groups/{group_id}/plan` | Compute provider-neutral additional Worker recommendations. |
| `GET /capacity/v1/worker-hosts` | List Worker instances for inventory and lifecycle reconciliation. |
| `GET /capacity/v1/worker-hosts/{instance_id}` | Read one Worker instance. |
| `POST /capacity/v1/worker-hosts/{instance_id}/drain` | Start claim- and epoch-fenced drain. |
| `POST /capacity/v1/worker-hosts/{instance_id}/lost` | Confirm that the provider host is absent. |

Worker group, pool, and instance IDs are canonical UUIDv7 values. `region_id`,
names, and provider `resource_id` values are opaque canonical strings.

### Resolve and plan

Resolve the group and pool first. Then ask Helmr how many additional Workers
each provider pool can use:

```sh
printf 'Authorization: Bearer %s\n' "$CAPACITY_TOKEN" |
  curl --fail-with-body --get \
  --header @- \
  --header 'Accept: application/json' \
  --data-urlencode "region_id=$REGION_ID" \
  --data-urlencode "name=$GROUP_NAME" \
  "$CONTROL_PLANE_URL/capacity/v1/worker-groups/resolve"

printf 'Authorization: Bearer %s\n' "$CAPACITY_TOKEN" |
  curl --fail-with-body \
  --request POST \
  --header @- \
  --header 'Accept: application/json' \
  --header 'Content-Type: application/json' \
  --data '{"pools":[{"pool_id":"019...","max_additional_workers":10}]}' \
  "$CONTROL_PLANE_URL/capacity/v1/worker-groups/$GROUP_ID/plan"
```

The plan response has this shape:

```json
{
  "worker_group_id": "019...",
  "worker_group_name": "default",
  "region_id": "aws-us-east-1",
  "group_status": "active",
  "pools": [
    {
      "pool_id": "019...",
      "pool_name": "default",
      "recommended_additional_workers": 2,
      "compatible_queued_items": 2,
      "registering_workers": 0,
      "active_workers": 1,
      "complete": true,
      "saturated": false,
      "scale_in_blocked": false
    }
  ],
  "unmatched_demand": [],
  "complete": true,
  "computed_at": "2026-08-29T00:00:00Z"
}
```

Treat the recommendation as input to your provider policy, not as an imperative
desired count. The scaler decides mutation rate, limits, and provider calls.

Scale out can use positive `recommended_additional_workers` subject to provider
limits. Scale in needs a complete, internally consistent observation. Do not
start a drain unless all of these are true:

- the group plan has `complete: true` and `unmatched_demand` is empty;
- the selected pool has `complete: true`, `saturated: false`,
  `scale_in_blocked: false`, and `compatible_queued_items: 0`;
- the plan's active/registering counts match Worker inventory observed in the
  same reconciliation cycle;
- inventory did not reach the requested limit, no Worker is registering or
  already draining, and no provider mutation is in flight; and
- provider inventory is complete and stable under its scale-in protection.

The drain fence and the advisory queued-demand check for `idle_scale_in` are final safety checks. They
do not make a partial plan or incomplete inventory safe for scale-in.

### Bound optional work per Worker

Read or replace a pool's policy at
`GET /capacity/v1/worker-groups/{group}/pools/{pool}/resource-policy` and
`PUT /capacity/v1/worker-groups/{group}/pools/{pool}/resource-policy`.
The replacement requires every field and the revision returned by the read:

```json
{
  "expected_revision": 1,
  "optional": {
    "cpu_millis": 0,
    "memory_bytes": 0,
    "disk_bytes": 0,
    "vm_slots": 0,
    "max_tasks": 0,
    "io_bytes_per_second": 0
  },
  "local_headroom_slots": 0
}
```

All limits default to zero. Optional seed generation, prepared Computers and
cache share these per-Worker ceilings. `local_headroom_slots` keeps full VM shapes
available to real work by restricting optional admission. It does not request
additional hosts or guarantee fleet-wide ready capacity. Set a provider minimum
size when physical standing capacity is required. Replacing policy requires the
current revision; a stale revision returns HTTP 409. A successful update records
desired policy, not proof that every Worker has finished reclaiming resources.

Each pool plan reports `policy_revision`, `placements.immediate`,
`placements.after_optional_reclaim`, `optional_occupancy`, `reclaiming_workers`,
`local_headroom_slots_in_use` and `credited_hosts` (host IDs and Worker epochs).
Placements count complete compatible VM shapes before queued work is packed.
Compatible pools can credit the same Worker, so their counts must not be added.
Missing or stale resource observations make immediate placement and optional
occupancy `null` and the plan incomplete. Post-reclaim counts remain advisory;
physical cleanup must finish before a granted allocation starts. Optional backlog
and occupancy do not create additional-Worker recommendations.

### Retry scale-in safely

Submit `POST /capacity/v1/worker-groups/{group}/pools/{pool}/scale-in` with
`{"operation_key":"<UUIDv7>"}`. Helmr selects an eligible host and commits its
drain together with a durable receipt. A retry with the same key returns that
selection; reusing it for another pool is a conflict. Only one unsettled automatic
scale-in operation is allowed per group. Hosts occupied only by optional work may
be selected, but cleanup must still finish before provider termination.

Read `GET /capacity/v1/worker-groups/{group}/scale-in-operations/pending` before
starting a new operation, including after controller restart. It returns
`{"operation":null}` or the outstanding receipt. Read a particular key at
`GET /capacity/v1/worker-groups/{group}/scale-in-operations/{operation_key}`.
Receipts retain the selected host, provider resource and drain claim. Their phase
is `draining`, `termination_ready`, `blocked` or `completed`. Terminate only the
exact resource with a current `termination_ready` receipt and matching inventory;
a blocked receipt requires reconciliation. Completion follows confirmed physical
provider absence. Response loss is not permission to select another host.

### React to capacity changes

Connect to `GET /capacity/v1/updates` with the capacity token and
`Accept: text/event-stream`. The stream emits `event: change` on connection and
on committed capacity-relevant changes, with heartbeat comments during idle
periods. Treat each event as an invalidation hint and re-read durable state.
Events are coalesced, have no replay cursor, and do not contain a capacity plan.
Reconnect after disconnects and reconcile periodically to cover lost hints and
provider-only changes. Save and output progress alone do not trigger capacity
notifications.

### Select a primary pool

Use the `claim_version` returned by Worker-group resolution:

```json
{
  "expected_group_claim_version": 4,
  "minimum_ready_hosts": 1,
  "pool_id": "019..."
}
```

`PUT .../primary-pool` returns `worker_group` and `applied`. Re-read the group
and retry from current state when the claim fence is stale. Selection checks
fresh, active, unpaused hosts at the current epoch under the same Group, Pool
and Host fences used by drain. Selecting the current primary is an exact replay;
it does not establish current fleet health.

Group and Pool resolution includes `retained_profiles`: up to 100 distinct
profiles with live Instance, creating checkpoint, parked checkpoint and eligible
Pool counts. Eligible Pool counts include the inspected Pool when it is active;
they are not a count of alternatives after its retirement. `complete: false` means more profiles exist; a truncated result is
not proof that retirement is safe. Valid parked checkpoints retain their profile
when waits resolve or Commands queue. A Pool also reports its claim version and
seal time. Retirement checks the complete database state atomically.

`GET /capacity/v1/deployment` reports this Control Plane replica's release,
source commit, worker protocol revision, applied database schema version and
migration dirty flag. It reads database state directly and does not report
provider fleet convergence.

### Reconcile Worker inventory

`GET /worker-hosts` accepts these optional query parameters:

| Parameter | Meaning |
| --- | --- |
| `worker_group_id` | Limit results to one group. |
| repeated `resource_id` | Match provider resource IDs. Each value is at most 512 bytes. |
| repeated `status` | Match `registering`, `active`, `draining`, `termination_ready`, or `lost`. |
| `has_unreclaimed_instance=true` | Return worker hosts that still own unreclaimed Computer Instance state. |
| `limit` | Result limit; default 200, maximum 500. |

The response contains `worker_hosts`. Each item includes `id`,
`resource_id`, `worker_group_id`, `worker_pool_id`, `status`, `claim_version`,
`created_at`, and `updated_at`. `drain_blockers` counts unreclaimed Instances and unreconciled Session
processes. Zero counts still require the worker's local cleanup acknowledgment.
`current_epoch`, `draining_at`, `drain_reason`,
`termination_ready_at`, and `lost_at` appear when applicable.

Group statuses are `active`, `paused`, `draining`, or `disabled`. Pool statuses
are `pending`, `active`, `draining`, or `disabled`.

### Drain before termination

Never terminate a provider host until its Worker instance reaches
`termination_ready`. Start drain with the current epoch and claim version:

```sh
printf 'Authorization: Bearer %s\n' "$CAPACITY_TOKEN" |
  curl --fail-with-body \
  --request POST \
  --header @- \
  --header 'Accept: application/json' \
  --header 'Content-Type: application/json' \
  --data "{\"expected_epoch\":$CURRENT_EPOCH,\"expected_claim_version\":$CLAIM_VERSION,\"reason\":\"idle_scale_in\"}" \
  "$CONTROL_PLANE_URL/capacity/v1/worker-hosts/$INSTANCE_ID/drain"
```

The epoch and claim version fence the exact Worker ownership observed by the
scaler. On conflict, discard the attempted mutation, read the instance again,
and reconcile from current state. For reason `idle_scale_in`,
Helmr returns `409` with code `queued_demand_present` instead of beginning
scale-in while that group has queued work. This demand snapshot is advisory;
new work may enqueue afterward. Use `replacement` or `capacity_reduction` for
planned maintenance that must preserve queued work while draining. The first
committed drain reason and epoch remain unchanged on replay.

After the provider confirms that a host is absent, call `POST .../{id}/lost`.
This is a confirmation of observed provider state, not a request for Helmr to
delete the host.

## Errors and retries

Non-2xx responses use the standard error envelope:

```json
{
  "error": {
    "code": "queued_demand_present",
    "message": "queued demand is present"
  }
}
```

`details` is optional and appears only when an error supplies structured
details.

Retry reads and reconciliation loops from freshly observed state. Do not retry
a provider mutation blindly. `404` with `not_found` means the referenced object
is absent. A stale drain or claim fence returns `409`; re-read before deciding
whether any provider action is still needed. Mutating request bodies are
limited to 16 KiB.

## Compatibility

`/capacity/v1` is the supported integration boundary. Additive response
fields may appear within `v1`; clients must ignore unknown fields. A breaking
wire change requires a new route version.

The historical Go module release `capacity/v0.1.0` remains downloadable for
reproducible builds, but it is frozen and receives no successor releases. New
scalers should implement this HTTP protocol directly.

## Rotate the token

Control Plane accepts one token and reads it at startup, so rotation has no
overlap window:

1. Pause the scaler.
2. Generate and store a new token.
3. Update `CAPACITY_TOKEN`, replace every Control Plane replica, and wait until
   all old replicas have stopped and all replacements are ready.
4. Update the scaler secret and restart the scaler.
5. Confirm the scaler can resolve a Worker group and read a plan, then resume.

The old token stops working only after the last old Control Plane replica has
stopped.

The capacity authority can create a pending pool with
`POST /capacity/v1/worker-groups/{workerGroupID}/pools` using `name` and
`expected_group_claim_version`, and disable an eligible pool with
`POST /capacity/v1/worker-groups/{workerGroupID}/pools/{workerPoolID}/disable`
using `expected_pool_claim_version`. These use the same group/pool locks,
claim checks, primary protection and custody checks as administration. Both
return the capacity WorkerPool projection. A disabled name cannot enroll again;
use a new generation name. Controllers must reobserve after a conflict or lost
response and must not infer a terminal fence from provider absence alone.
