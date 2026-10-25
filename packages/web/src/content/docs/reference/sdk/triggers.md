---
title: Triggers
description: triggers.cron() and the read-only client Trigger methods.
sidebarLabel: Triggers
---

# Triggers

This page lists the trigger declaration API and the client methods that read
the Trigger resource. A Trigger is one activation of a declared trigger: each
promotion starts a new one. For how triggers fire and follow Deployments, see [Triggers](/docs/concepts/triggers).

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
  template: reporting, // an existing template() definition
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

## Client: Triggers

```ts
client.triggers.get(triggerId: string, options?): Promise<Trigger>
client.triggers.list(query?: { agent?: string; cursor?: string; limit?: number }, options?): Promise<CursorPage<Trigger>>
```

`agent` is the identifier declared in code.
List returns current activations. An unknown `agent`, or one absent from the
current Deployment, returns an empty page. `get` can read a retained historical
activation by its ID. The TypeScript client currently exposes only `agent`,
`cursor` and `limit` for listing; use the [CLI](/docs/reference/cli/trigger) or
[REST API](/docs/reference/api/overview#trigger-activations-and-occurrences) for
historical listing, logical-key filtering and occurrence outcomes. See [Agents](/docs/reference/sdk/agent#client-agents).
`limit` is 1–100.

| `Trigger` field | Type | Description |
| --- | --- | --- |
| `id` | `string` | ID of this activation. A new promotion creates a new ID. |
| `agent` | `string` | Agent identifier declared in code. |
| `deploymentId` | `string` | Deployment that declared the trigger. |
| `triggerKey` | `string` | The trigger `id`. |
| `input` | `InputContent` | Declared input. |
| `cron` | `{ pattern, timezone }` | Declared expression and timezone. |
| `activeFrom` | `string` | When this Trigger became active. |
| `activeUntil` | `string`, optional | When it stopped being active. |
| `nextFireAt` | `string`, optional | Next planned fire time. |

Triggers are read-only. Change triggers by editing the Agent definition and
deploying.
