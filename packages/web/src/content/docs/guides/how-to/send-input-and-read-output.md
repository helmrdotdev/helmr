---
title: Send input and read output
description: Continue an Actor Session and page through its durable output.
---

# Send input and read output

Send one JSON value to an open Session:

```sh
helmr actor input send SESSION_ID \
  --project agents --env development \
  --input-json '{"type":"instruction","text":"also update the tests"}' \
  --idempotency-key slack:T123:C456:1712345678.000100
```

Use a stable upstream event identifier as the idempotency key. A retry then
replays the accepted append instead of adding a duplicate command.

Read one finite output page:

```sh
helmr actor output read SESSION_ID \
  --project agents --env development \
  --after 0 --limit 50 --json
```

Each record has a durable `sequence`, data, content type, timestamp, and Run
provenance. Pass the last sequence back as `--after` to continue. Multiple
readers can keep independent cursors.

With the SDK:

```ts
const session = client.sessions.ref("SESSION_ID")
await session.input.send(
  { type: "instruction", text: "also update the tests" },
  { idempotencyKey: "slack:T123:C456:1712345678.000100" },
)

const page = await session.output.list({
  after: 0,
  limit: 50,
})
for (const record of page.records) {
  console.log(record.sequence, record.data)
}
```

Run logs are execution telemetry. Session output is the Actor's durable
application protocol; do not substitute one for the other.
