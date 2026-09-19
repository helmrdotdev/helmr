---
title: Send input and read output
description: Continue an Actor Session and page through its durable output.
---

# Send input and read output

Send one JSON value to an open Session:

```sh
helmr actor send SESSION_ID \
  --project agents --env development \
  --data-json '{"type":"instruction","text":"also update the tests"}' \
  --idempotency-key slack:T123:C456:1712345678.000100
```

Use a stable upstream event identifier as the idempotency key. A retry then
replays the original admission instead of adding duplicate work. `send` routes to
active ready work or enqueues when idle; use `enqueue` to always queue a new Turn.
For an answer or approval tied to existing work, use `session.turn(turnId).send`
or `helmr actor turn send SESSION_ID TURN_ID`. These never retarget a late reply.

Read one finite event page:

```sh
helmr actor events SESSION_ID \
  --project agents --env development \
  --after 0 --limit 50 --json
```

Each event has a durable `sequence`, `kind`, data, timestamp, nullable Turn ID
and nullable Run provenance. Pass `next_after` as `--after` to continue. Multiple
readers can keep independent cursors. Output EOF is not a terminal Turn outcome.

With the SDK:

```ts
const session = client.sessions.ref("SESSION_ID")
await session.send(
  { type: "instruction", text: "also update the tests" },
  { idempotencyKey: "slack:T123:C456:1712345678.000100" },
)

const page = await session.events.list({
  after: 0,
  limit: 50,
})
for (const record of page.records) {
  console.log(record.sequence, record.data)
}
```

Run logs are execution telemetry. Session output is the Actor's durable
application protocol; do not substitute one for the other.
