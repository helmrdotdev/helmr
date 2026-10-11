---
title: Triggers
description: triggers.cron() and the read-only client Schedule methods.
sidebarLabel: Triggers
---

# Triggers

This page lists the trigger declaration API and the client methods that read
the resulting Schedules. For how triggers fire and how Schedules follow
Deployments, see [Triggers](/docs/concepts/triggers).

## `triggers.cron()`

```ts
triggers.cron<I extends InputContent>(
  id: string,
  expression: string,
  options: {
    readonly timezone: string
    readonly input: I
    readonly slack?: Readonly<{ channelId: string }>
  },
): CronTrigger<I>

interface CronTrigger<I extends InputContent = InputContent> {
  readonly id: string
  readonly cron: string
  readonly timezone: string
  readonly input: I
  readonly slack?: Readonly<{ channelId: string }>
}
```

| Argument | Required | Description |
| --- | --- | --- |
| `id` | yes | Trigger ID, unique within the Agent. |
| `expression` | yes | Five-field cron expression. |
| `options.timezone` | yes | IANA timezone, for example `"America/New_York"`. |
| `options.input` | yes | Input for each fired Turn: an array of text parts. |
| `options.slack.channelId` | no | Raw Slack channel ID (for example `C0123456789`) to open each fired Session in. The object must contain only `channelId`. See [Slack](/docs/concepts/slack). |

Throws when `id`, `expression` or `timezone` is blank, when `input` is not a
text-part array, or when `slack` is malformed.

Add triggers to an Agent with its `triggers` field:

```ts
import { agent, triggers } from "@helmr/sdk"

export const dailyReport = agent({
  id: "daily-report",
  computer: reporting, // an existing computer() definition
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

Each fire starts an independent Session on a new Computer. The handler receives
the declared `input`; there is no separate schedule payload.

## Client: Schedules

```ts
client.schedules.get(scheduleId: string, options?): Promise<Schedule>
client.schedules.list(query?: { agentId?: string; cursor?: string; limit?: number }, options?): Promise<CursorPage<Schedule>>
```

`agentId` is the Agent UUID (see `agentId` on
[Agent definitions](/docs/reference/sdk/agent#client-agent-definitions)).
`limit` is 1–100.

| `Schedule` field | Type | Description |
| --- | --- | --- |
| `id` | `string` | Schedule ID. |
| `agentId` | `string` | Agent UUID. |
| `deploymentId` | `string` | Deployment that declared the trigger. |
| `triggerKey` | `string` | The trigger `id`. |
| `input` | `InputContent` | Declared input. |
| `cron` | `{ pattern, timezone }` | Declared expression and timezone. |
| `activeFrom` | `string` | When this Schedule became active. |
| `activeUntil` | `string`, optional | When it stopped being active. |
| `nextFireAt` | `string`, optional | Next planned fire time. |

Schedules are read-only. Change triggers by editing the Agent definition and
deploying.
