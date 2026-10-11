---
title: Schedules
description: Declare cron triggers and inspect their active Deployment intervals.
---

# Schedules

Declare a trigger on an Agent:

```ts
import { agent, computer, image, triggers } from "@helmr/sdk"

const workspace = computer({
  id: "reporting",
  image: image("reporting").from("node:24-bookworm-slim"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const dailyReport = agent({
  id: "daily-report",
  computer: workspace,
  closeAfterIdle: "5m",
  triggers: [
    triggers.cron("morning", "0 9 * * *", {
      timezone: "America/New_York",
      input: [{ type: "text", text: "Prepare the daily report" }],
    }),
  ],
  async turn(turn) {
    return { requested: turn.input }
  },
})
```

`triggers.cron` requires a trigger ID, five-field expression, IANA timezone and
a JSON text-part input array. Optional `slack: { channelId: "C0123456789" }` selects
an actual Slack channel ID. Promotion verifies the Agent’s dedicated App connection
and channel access, then pins that connection. Each fire starts an independent
Session on a fresh Computer with an explicit Slack opening when routed;
the handler receives the declared input without a synthetic schedule payload.

`client.schedules.retrieve(id)` reads one Schedule.
`client.schedules.list({ agentId?, cursor?, limit? })` pages records; `agentId`
is an Agent UUID and limits range from 1 to 100. Results include `id`, `agentId`,
`deploymentId`, `triggerKey`, `input`, `cron`, `activeFrom`, optional `activeUntil`
and optional `nextFireAt`.

These APIs are read-only. Edit the Agent declaration and promote a Deployment
to change future triggers. Already-admitted Sessions retain their pinned code.

For a new Agent with Slack delivery, first promote it without the Slack-routed
trigger, connect its Slack app from the Agent page, then add the trigger and
promote again. Promotion verifies the connected app and channel before enabling
the schedule.
