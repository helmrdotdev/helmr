---
title: Schedule an Agent
description: Declare recurring inputs and inspect the reconciled Schedule.
---

# Schedule an Agent

Add a cron trigger to your Agent's `triggers` array:

```ts
import { triggers } from "@helmr/sdk"

export const morning = triggers.cron("morning", "0 9 * * *", {
  timezone: "America/New_York",
  input: [{ type: "text", text: "Prepare the daily report" }],
})
```

Use `triggers: [morning]` on the Agent definition. The
[complete example](/docs/reference/sdk/schedules) includes its Computer and Turn
handler. Configure any stable Secret bindings on the Computer definition.

Deploy and promote the project. Each scheduled fire admits a fresh independent
Session and Computer. The handler receives your declared input. Delivery retries
reconcile the scheduled occurrence instead of duplicating admission.

```sh
helmr schedule list --project agents --env production --json
helmr schedule get SCHEDULE_ID --project agents --env production --json
```

Change timing or input in source and promote a new Deployment. Removing the
trigger ends its active interval. Already-admitted work keeps its pinned code.
Select `closeAfterIdle` or explicitly close resulting Sessions for bounded
conversation lifetimes.
