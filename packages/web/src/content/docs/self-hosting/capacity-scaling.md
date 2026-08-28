---
title: Custom capacity scaling
description: Connect trusted provider-specific scaling automation to a self-hosted Helmr deployment.
---

# Custom capacity scaling

Build a provider-specific scaler against the Capacity deployment protocol at
`/api/capacity/v0`. Helmr supplies desired-capacity and Worker lifecycle state.
Your scaler owns provider policy and mutations, including creating and
terminating hosts.

This is a deployment infrastructure protocol for one trusted scaler. It is not
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
| `GET /api/capacity/v0/worker-groups/resolve?region_id=...&name=...` | Resolve a Worker group by canonical Region ID and name. |
| `GET /api/capacity/v0/worker-groups/{group_id}/pools/resolve?name=...` | Resolve a pool in a Worker group. |
| `PUT /api/capacity/v0/worker-groups/{group_id}/primary-pools` | Select the primary pool with a group claim-version fence. |
| `POST /api/capacity/v0/worker-groups/{group_id}/plan` | Compute provider-neutral additional Worker recommendations. |
| `GET /api/capacity/v0/worker-instances` | List Worker instances for inventory and lifecycle reconciliation. |
| `GET /api/capacity/v0/worker-instances/{instance_id}` | Read one Worker instance. |
| `POST /api/capacity/v0/worker-instances/{instance_id}/drain` | Start claim- and epoch-fenced drain. |
| `POST /api/capacity/v0/worker-instances/{instance_id}/lost` | Confirm that the provider host is absent. |

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
  "$CONTROL_PLANE_URL/api/capacity/v0/worker-groups/resolve"

printf 'Authorization: Bearer %s\n' "$CAPACITY_TOKEN" |
  curl --fail-with-body \
  --request POST \
  --header @- \
  --header 'Accept: application/json' \
  --header 'Content-Type: application/json' \
  --data '{"pools":[{"pool_id":"019...","max_additional_workers":10}]}' \
  "$CONTROL_PLANE_URL/api/capacity/v0/worker-groups/$GROUP_ID/plan"
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

The drain fence and `require_zero_queued_demand` are final safety checks. They
do not make a partial plan or incomplete inventory safe for scale-in.

### Select a primary pool

Use the `claim_version` returned by Worker-group resolution:

```json
{
  "expected_group_claim_version": 4,
  "pool_id": "019..."
}
```

`PUT .../primary-pools` returns `worker_group` and `applied`. Re-read the group
and retry from current state when the claim fence is stale.

### Reconcile Worker inventory

`GET /worker-instances` accepts these optional query parameters:

| Parameter | Meaning |
| --- | --- |
| `worker_group_id` | Limit results to one group. |
| repeated `resource_id` | Match provider resource IDs. Each value is at most 512 bytes. |
| repeated `status` | Match `registering`, `active`, `draining`, `termination_ready`, or `lost`. |
| `has_unreclaimed_runtime=true` | Return instances still owning unreclaimed runtime state. |
| `limit` | Result limit; default 200, maximum 500. |

The response contains `worker_instances`. Each item includes `id`,
`resource_id`, `worker_group_id`, `worker_pool_id`, `status`, `claim_version`,
`created_at`, and `updated_at`. `current_epoch`, `draining_at`,
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
  --data "{\"expected_epoch\":$CURRENT_EPOCH,\"expected_claim_version\":$CLAIM_VERSION,\"require_zero_queued_demand\":true}" \
  "$CONTROL_PLANE_URL/api/capacity/v0/worker-instances/$INSTANCE_ID/drain"
```

The epoch and claim version fence the exact Worker ownership observed by the
scaler. On conflict, discard the attempted mutation, read the instance again,
and reconcile from current state. When `require_zero_queued_demand` is true,
Helmr returns `409` with code `queued_demand_present` instead of beginning
scale-in while that group has queued work.

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

`/api/capacity/v0` is the supported integration boundary. Additive response
fields may appear within `v0`; clients must ignore unknown fields. A breaking
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
