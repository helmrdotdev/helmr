---
title: Answer questions
description: Find an Agent's pending questions and answer them from the CLI, the SDK or Slack.
---

# Answer questions

When an Agent calls `turn.ask`, its Turn waits until someone answers. This guide
finds pending questions and answers them. For how questions work, answer
controls and what the Agent receives, see [Questions](/docs/concepts/questions).

The examples use the `demo` Project's `staging` Environment, with the CLI
configured (see [Quickstart](/docs/quickstart)).

## Find the question

A new question appears in the Session's events as an `ask.created` record. Its
Turn ID is on the record, and its data contains the `ask_id` and the question:

```sh
helmr session events SESSION_ID --project demo --env staging --jsonl
```

You can also list a Turn's questions directly:

```sh
helmr session turn ask list --session SESSION_ID --turn TURN_ID \
  --project demo --env staging
```

Each line shows a question ID, its status (`pending`, `answered` or
`cancelled`) and its prompt. If you do not know the Turn, run
`helmr session turn list --session SESSION_ID` and check the Turns that are
still `running`.

## Read the question

```sh
helmr session turn ask get ASK_ID --session SESSION_ID --turn TURN_ID \
  --project demo --env staging --json
```

The `question` field says what shape the answer must have:

| Control | Answer JSON |
| --- | --- |
| `{"type":"text"}` | A JSON string, such as `"revision-42"`. |
| `{"type":"choice", ...}` | An object with a `selectedIds` array of option IDs. The server resolves their values. |
| Choice with `"multiple": true` | `selectedIds` may contain more than one option. |
| Choice with `"allowNotes": true` | Optional `"notes"` supplement an answer and do not replace a selection. |
| Choice with `"allowCustom": true` | Use `"custom"` for an alternative answer; a single choice cannot combine it with a selection. |

For example, for a choice question with options `approve` and `reject`:

```json
{ "selectedIds": ["approve"] }
```

## Answer it

```sh
helmr session turn ask answer ASK_ID --session SESSION_ID --turn TURN_ID \
  --project demo --env staging \
  --answer-json '"revision-42"' \
  --answer-id answer:issue-42:revision
```

Use `--answer-file answer.json` for longer answers. `--answer-id` identifies
this answer: if the command fails with an uncertain result, run it again with the
same answer ID and the same answer.

The answer is recorded under the identity that sent it. A signed-in member of
the organization can answer. An API key needs the `asks:answer` permission.
Once the question is answered, the Agent's `turn.ask` call returns and the Turn
continues; follow it as described in
[Start and observe a Session](/docs/guides/start-and-observe-a-session).

A message sent with `helmr session turn send` does not answer a
question. Use the answer command.

## Answer from the SDK

```ts
import { HelmrClient } from "@helmr/sdk"

const client = new HelmrClient({
  url: process.env.HELMR_API_URL!,
  apiKey: process.env.HELMR_API_KEY!,
})

const turn = client.sessions.ref("SESSION_ID").turns.ref("TURN_ID")
const page = await turn.asks.list({ limit: 50 })
const pending = page.items.find(ask => ask.status === "pending")

if (pending?.question?.type === "text") {
  await turn.asks.answer(pending.id, {
    answer: "revision-42",
    answerId: "answer:issue-42:revision",
  })
}
```

`turn.asks.get(id)` returns one question with its `question`,
`status` and, once answered, `answer`.

## Answer in Slack

When a Session is connected to a Slack conversation, its questions are posted in
the thread and people can answer them there. Each person first links their Slack
account to their Helmr account. See [Slack](/docs/concepts/slack) and
[Connect an Agent to Slack](/docs/guides/connect-slack).

The Console shows a Session's questions and their status on the Session page, with
the CLI commands to answer them.
