---
title: Inspect a Turn
description: Observe progress, outstanding questions and a retained terminal outcome.
---

# Inspect a Turn

Use the Session and Turn IDs returned by Agent start or enqueue:

```sh
helmr session get SESSION_ID --project agents --env development --json
helmr session turn get SESSION_ID TURN_ID --project agents --env development --json
helmr session events SESSION_ID --project agents --env development --after 0 --json
helmr session turn wait SESSION_ID TURN_ID --project agents --env development --timeout 10m --json
```

A Turn can be queued, running or finalizing before it settles. Read events while
waiting so progress remains visible. Handler return and output EOF are not
completion. Finalization may include callback/question drainage or saving; do not
infer a storage problem simply from the absence of output.

Inspect outstanding questions with:

```sh
helmr session turn ask list SESSION_ID TURN_ID --project agents --env development --json
```
 Respond through the exact ask route. Inspect Session holds to identify
any required resume operation and its owning Session.

```ts
import { HelmrClient } from "@helmr/sdk"

const url = process.env["HELMR_API_URL"]
if (!url) throw new Error("Set HELMR_API_URL to your control plane URL")
const client = new HelmrClient({ url, apiKey: process.env["HELMR_API_KEY"]! })
const session = client.sessions.get("SESSION_ID")
const turn = session.turn("TURN_ID")
const observation = turn.wait({ timeout: "10m" })
const events = await session.events.list({ after: 0, limit: 100 })
const asks = await turn.asks.list({ limit: 50 })
const result = await observation
```

Timeout stops only observation. A settled outcome can be completed, failed,
interrupted or cancelled. Retained state includes available result, response,
error and payload expiry. Process-wide stdout/stderr is internal; use authored
Turn output for user-visible progress and Computer Command logs for commands.
