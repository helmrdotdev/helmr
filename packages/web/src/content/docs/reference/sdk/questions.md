---
title: Questions
description: Ask for typed answers with exact Turn identity and authenticated attribution.
---

# Questions

Use `turn.ask` to ask a person a question from an active Turn and await their
answer. A prompt is Content:

```ts
import type { Turn } from "@helmr/sdk"

export async function requestApproval(turn: Turn) {
  const response = await turn.ask({
    prompt: [{ type: "text", text: "Publish this revision?" }],
    answer: {
      type: "choice",
      options: [
        { id: "approve", label: "Approve", value: true },
        { id: "decline", label: "Decline", value: false },
      ],
    },
  })
  return response
}
```

The result is `{ answer, respondedBy: { kind, id } }`. Responder kind is `user`
or `api_key`. Supported controls are `{ type: "text" }`
and `{ type: "choice", options, multiple?, allowText? }`.

Text answers are strings. Choice answers have
`{ selected: [{ id, value }], text? }`. Include exact declared IDs and values,
in declaration order without duplicates. `multiple` permits more than one
selection; `allowText` permits custom text. At least one selection or nonempty
custom text is required.

## External observation and response

```ts
import { HelmrClient } from "@helmr/sdk"

const url = process.env["HELMR_API_URL"]
if (!url) throw new Error("Set HELMR_API_URL to your control plane URL")
const client = new HelmrClient({ url, apiKey: process.env["HELMR_API_KEY"]! })
const turn = client.sessions.get("SESSION_ID").turn("TURN_ID")
const page = await turn.asks.list({ limit: 50 })
const question = await turn.asks.get("ASK_ID")
const receipt = await turn.asks.respond("ASK_ID", {
  answer: { selected: [{ id: "approve", value: true }] },
  responseId: "response:revision-42:approve",
})
```

The IDs above stand for the exact admitted Session, Turn and ask. Render the
retained prompt/control before constructing an answer. Retry an uncertain
response with the same response ID and answer. Current Organization membership
or explicit API-key answer permission governs responses; viewing a question in
Slack is not itself answer authority.

Questions are pending, responded or cancelled. Payload expiry is explicit;
identity can remain after prompt/answer content expires. Interruption and
cancellation can withdraw a question. A stale answer cannot authorize a new Turn
or a different native callback.

Await every question you start. Returning with forgotten/unjoined asks is a
contract error. Handler return closes new asks, while already-admitted questions
may resolve during callback drainage. Native permission adapters must validate
the exact live request and propagate cancellation through the native harness.
A human answer alone does not override native or Helmr execution authority.

A question is limited to 256 KiB and its prompt to 64 Content parts. Answers
are limited to 64 KiB. Choice controls allow up to 100 options, with IDs up to
128 bytes, labels up to 256 bytes and descriptions up to 2 KiB. Custom choice
text is limited to 8 KiB. Keep credentials out of all question content.

## Answer from Slack or the CLI

Slack shows the complete prompt and choice labels/descriptions. Its Answer button
opens a native form when the question fits Slack's limits. Every question also
includes CLI commands for the exact Session, Turn and question. Use them when the
form is unavailable or your answer exceeds Slack's 3,000-character input limit.

The commands identify the Helmr origin, Project and Environment and use your
`helmr login`. Unset `HELMR_API_KEY` for that login-based flow. Inspect the question
with `helmr session turn ask get`, then save your answer in a JSON file: a JSON
string for text, or `{ "selected": [{ "id": "approve", "value": true }] }` for a
choice. Use the declared IDs and values in declaration order; optional custom
`text` requires `allowText`. Submit with `--answer-file` and a unique
`--response-id`, reusing the same ID and unchanged answer for an uncertain retry.
An environment-scoped API key can also answer with its existing permission; omit
`--project` and `--env` when using that authentication method.

Console displays question details, recorded answers and attribution. Responses
are submitted through Slack or the CLI. The response instructions disappear when
the question is answered or withdrawn; current authority still governs any saved
CLI command.
