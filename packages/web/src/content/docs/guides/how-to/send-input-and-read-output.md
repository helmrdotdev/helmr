---
title: Send input and read output
description: Continue a Session and reconnect to its durable timeline.
---

# Send input and read output

Enqueue one JSON input for a new Turn:

```sh
helmr session enqueue SESSION_ID \
  --project agents --env development \
  --input-json '[{"type":"text","text":"also update the tests"}]' \
  --idempotency-key request:revision-42:tests
```

Reuse the same key and input for retries of that request. To steer current work,
use `helmr session send ... --data-json JSON`; it targets active work or enqueues
when idle. Exact `helmr session turn send SESSION_ID TURN_ID --data-json JSON`
never retargets another Turn. Message acceptance is separate from delivery and
application. Held Sessions reject automatic send. Questions use the
[exact ask response API](/docs/guides/how-to/wait-for-human-input).

Read a finite event page while work is running or finalizing:

```sh
helmr session events SESSION_ID --project agents --env development \
  --after 0 --limit 50 --json
```

Pass `next_after` unchanged as the next `--after`. Readers can keep independent
cursors. Each record has a sequence, kind, data, creation time, Session ID and
nullable Turn ID. Retention metadata identifies unavailable earlier history.

```ts
import { HelmrClient } from "@helmr/sdk"

const url = process.env["HELMR_API_URL"]
if (!url) throw new Error("Set HELMR_API_URL to your control plane URL")
const client = new HelmrClient({ url, apiKey: process.env["HELMR_API_KEY"]! })
const session = client.sessions.get("SESSION_ID")
const turn = await session.enqueue([{ type: "text", text: "also update the tests" }], {
  idempotencyKey: "request:revision-42:tests",
})
const observation = turn.wait({ timeout: "10m" })
const page = await session.events.list({ after: 0, limit: 50 })
for (const record of page.records) {
  console.log(record.sequence, record.kind, record.data)
}
const outcome = await observation
```

Follow pages or use `session.events.stream` for continuing progress. A page ending
or output EOF is not completion. Read the Turn outcome for result and response;
a timed observation does not cancel execution. Process diagnostics are internal,
and authored content should never expose credentials or private native payloads.
