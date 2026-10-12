---
title: Triggers
description: Declare cron triggers in source; each fire starts a fresh Session on a fresh Computer.
---

# Triggers

A trigger starts work on a schedule. You declare cron triggers on an Agent in
source, and promoting a Deployment makes them active in that Environment. Each
fire starts a new Session with the input you declared.

```ts
import { agent, template, image, triggers } from "@helmr/sdk"

const reporting = template({
  id: "reporting",
  image: image("reporting").from("node:24-bookworm-slim"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const dailyReport = agent({
  id: "daily-report",
  template: reporting,
  closeAfterIdle: "5m",
  triggers: [
    triggers.cron("morning", "0 9 * * 1-5", {
      timezone: "America/New_York",
      input: [{ type: "text", text: "Prepare the daily report" }],
    }),
  ],
  async turn(turn) {
    await turn.respond("Report ready")
    return { requested: turn.input }
  },
})
```

`triggers.cron(id, expression, { timezone, input, slack? })` takes:

| Argument | Meaning |
| --- | --- |
| `id` | Trigger ID, unique within the Agent. |
| `expression` | A standard five-field cron expression: minute, hour, day of month, month, day of week. |
| `timezone` | An exact IANA timezone name, such as `"Europe/London"`. The expression is evaluated in this timezone. |
| `input` | The Turn input: a JSON array of text parts. |
| `slack` | Optional `{ channelId }`. Posts each fire's Session to a Slack channel. |

The expression must have occurrences. When a daylight-saving change repeats a
local time, the trigger fires once, at the first instance.

## What a fire does

Each fire starts an independent Session on a fresh
[Computer](/docs/concepts/computers) created from the Agent's Computer
definition. The first [Turn](/docs/concepts/turns) receives the declared
`input` unchanged; there is no extra schedule payload and no human requester.
`turn.source.kind` is `"schedule"`.

Fires do not share a Session or a Computer. To carry state from one run to the
next, store it outside the Computer, or have the Turn start work on a
long-lived Computer through the [client](/docs/reference/sdk/client).

The Session then follows the Agent's ordinary lifetime rules. Set
`closeAfterIdle` on the Agent so scheduled Sessions close soon after their
work, or close them explicitly. See [Sessions](/docs/concepts/sessions).

Each scheduled occurrence starts at most one Session. Its identity is the
Environment, Agent, trigger ID and scheduled instant, so retries inside the
scheduler do not start the work twice.

## Missed runs

Helmr does not backfill missed occurrences:

- After a scheduler delay or outage, only the latest due occurrence within
  five minutes of its scheduled time starts. Earlier due occurrences are
  recorded as missed.
- Times when the Environment had no promoted Deployment containing the
  trigger are never run.
- An occurrence that cannot be admitted, for example because its Slack
  channel is no longer available, is recorded as rejected and is not retried.
  A temporary shortage, such as capacity, does not reject it; Helmr keeps
  trying while the occurrence is within that five-minute window.

## Slack delivery

With `slack: { channelId: "C0123456789" }`, each fire opens a new thread in
that channel and binds the Session to it. `channelId` is a raw Slack channel
ID. Promotion checks that the Agent's Slack app is connected and can use the
channel, then pins that connection for the trigger. If the connection is later
replaced, promote again. See [Slack](/docs/concepts/slack).

## Changing schedules

Triggers are defined only in source. To add, change or remove a trigger, edit
the Agent and deploy a new Deployment, then promote it (`helmr deploy`
promotes by default). Promotion closes the previous triggers' active interval
and starts a new one for the triggers in the promoted Deployment. Removing a
trigger stops future fires. Sessions already started keep running on the
Deployment they were started with.

## Reading schedules

Each promoted trigger appears as a read-only Schedule with its cron pattern,
timezone, input, active interval and next fire time:

```sh
helmr schedule list --json
helmr schedule get SCHEDULE_ID --json
```

In TypeScript, use `client.schedules.list()` and `client.schedules.get(id)`.
See the [triggers reference](/docs/reference/sdk/triggers) and
[`helmr schedule`](/docs/reference/cli/schedule).
