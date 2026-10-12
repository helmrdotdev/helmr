---
title: Build a durable Agent
description: Build an Agent that keeps live state and files across the Turns of one Session.
---

# Build a durable Agent

This tutorial builds an Agent that remembers what happened earlier in its
Session. It keeps a live object from `init` in memory and a log file on the
Computer's disk, then continues the conversation across several Turns, an idle
period and an interruption. It needs no model provider.

You need a project created with `helmr init` and the CLI configured (see
[Quickstart](/docs/quickstart)). The examples use the `demo` Project's `staging`
Environment. For the guarantees behind each step, see
[Durability](/docs/concepts/durability).

## Define the Agent

Create `agents/assistant.ts`:

```ts
import { agent, template, image } from "@helmr/sdk"
import { appendFile, readFile } from "node:fs/promises"

const LOG = "/workspace/log.txt"

export const assistantComputer = template({
  id: "assistant",
  image: image("assistant").from("node:24-bookworm-slim"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const assistant = agent({
  id: "assistant",
  template: assistantComputer,
  closeAfterIdle: "1d",
  maxTurnDuration: "10m",

  async init(ctx) {
    // Runs once per Session process, before its first Turn.
    return { turns: 0, startedAs: ctx.recovery.kind }
  },

  async turn(turn, ctx) {
    const text = turn.input.map(part => part.text).join("").trim()
    if (!text) throw new Error("Send some text")

    ctx.initResult.turns += 1
    await appendFile(LOG, `${turn.sequence}: ${text}\n`, { flush: true })
    const lines = (await readFile(LOG, "utf8")).trimEnd().split("\n").length

    await turn.output.write(`Recorded "${text}"`)
    await turn.respond(`Turn ${ctx.initResult.turns} in this process. Log entries: ${lines}.`)
    return { turnsInProcess: ctx.initResult.turns, logEntries: lines, startedAs: ctx.initResult.startedAs }
  },
})
```

The Agent keeps two kinds of state:

- `ctx.initResult` is the value `init` returned. It is an ordinary live object;
  it could just as well hold an open client or a child process. It lasts as long
  as the Session's process.
- `/workspace/log.txt` is a file on the Computer's disk. The disk is saved after
  every Turn, and a Turn is `completed` only once that save is stored.

`closeAfterIdle` closes the Session after a day without work, and
`maxTurnDuration` limits each Turn. See [Agents](/docs/concepts/agents).

## Deploy and start

```sh
helmr deploy . --project demo --env staging
helmr session create assistant --project demo --env staging \
  --text "first" --key user:ada \
  --idempotency-key tutorial:assistant:1
```

Save the `session_id` from the receipt. The session key `user:ada` names this
conversation: starting `assistant` again with the same key adds input to the same
Session instead of creating a new one.

Wait for the Turn:

```sh
helmr session turn wait TURN_ID --session SESSION_ID --timeout 10m \
  --project demo --env staging
```

The response reads `Turn 1 in this process. Log entries: 1.`

## Continue the Session

```sh
helmr session enqueue SESSION_ID --project demo --env staging \
  --text "second" --idempotency-key tutorial:assistant:2
```

Wait for the new `turn_id`. The response now reads
`Turn 2 in this process. Log entries: 2.` The counter in `initResult` kept its
value because `init` did not run again.

Leave the Session alone for a while, then enqueue `third`. When a Session has
no work, Helmr hibernates its Computer with memory, processes and disk, and the
next input resumes it where it stopped. The counter continues at 3 and
`startedAs` is still `initial`.

Follow progress at any time with:

```sh
helmr session events SESSION_ID --project demo --env staging
```

## When the process is lost

If the Session's process is lost while a Turn runs, that Turn ends
`interrupted` and the Session is held. Helmr does not replay the input.
Find the hold and resume:

```sh
helmr session get SESSION_ID --project demo --env staging
helmr session resume SESSION_ID --hold HOLD_ID --project demo --env staging
```

On the next input Helmr starts a new process on the same Computer and runs
`init` again with `ctx.recovery.kind` set to `reconstructed`. In this Agent the
counter starts again at 1, while the log file still has every entry the disk
kept. That is the rule to design for: keep anything that must survive
reconstruction on disk or in your own store, and make external effects safe to
repeat. Losing the worker of a running Computer is different: that Computer
cannot be recovered. See [Durability](/docs/concepts/durability).

## Interrupt and resume

You can stop work yourself the same way:

```sh
helmr session interrupt SESSION_ID --project demo --env staging
```

Interrupt stops the running Turn, keeps queued Turns waiting and holds the
Session. The receipt includes `hold_id`; resume with it when you want queued
work to continue:

```sh
helmr session resume SESSION_ID --hold HOLD_ID --project demo --env staging
```

Resuming lets queued and new Turns run. It does not rerun the interrupted input;
send it again if you still want it. See [Sessions](/docs/concepts/sessions).

## Finish

```sh
helmr session close SESSION_ID --project demo --env staging
```

Close finishes work already accepted and then ends the Session. It does not
release holds; resume first if the Session is held.

## Next steps

- Start a model client or coding-agent process in `init` and reuse it in every
  Turn; see [Agents](/docs/concepts/agents).
- Ask a person for input with `turn.ask`; see
  [Answer questions](/docs/guides/answer-questions).
- Set up the Computer's tools and files once with `prepare`; see
  [Prepare a Computer](/docs/guides/prepare-a-computer).
