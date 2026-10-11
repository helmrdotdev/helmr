---
title: Start an Agent
description: Admit a first Turn with default or explicit Computer placement.
---

# Start an Agent

Start a deployed Agent by its authored definition ID:

```sh
helmr agent start reviewer \
  --project agents --env development \
  --input-json '[{"type":"text","text":"Review issue 42"}]' \
  --session-key review:42 \
  --idempotency-key review:42:first \
  --json
```

Use `--input-file input.json` for file input. A JSON text-part array is required; use `[]`
when no input text is needed. The receipt contains Session and Turn IDs.
Omitting `--computer` creates a Computer from the Agent's definition. Supply an
existing Computer ID for explicit placement.

The Session key selects a conversation. The idempotency key identifies the
request; reuse it with identical arguments after an uncertain response. New
work uses a new key. Existing Sessions remain pinned to their original Deployment.

```ts
import { HelmrClient } from "@helmr/sdk"

const url = process.env["HELMR_API_URL"]
if (!url) throw new Error("Set HELMR_API_URL to your control plane URL")
const client = new HelmrClient({ url, apiKey: process.env["HELMR_API_KEY"]! })
const { session, turn } = await client.agents.start("reviewer", {
  input: [{ type: "text", text: "Review issue 42" }],
  sessionKey: "review:42",
  idempotencyKey: "review:42:first",
})
const observation = turn.wait({ timeout: "10m" })
const progress = await session.events.list({ after: 0 })
const result = await observation
```

Admission does not establish dispatch or completion. Read events while observing
the Turn. Timeout does not cancel it. See [Inspect a Turn](/docs/guides/how-to/inspect-a-turn).
