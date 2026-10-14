---
title: Questions
description: Ask a person a typed question from a running Turn and wait for an attributed answer.
---

# Questions

A question lets a running [Turn](/docs/concepts/turns) ask a person for input
and wait for the answer. Each question belongs to one Session and one Turn, has
its own ID, declares the shape of the answer it accepts and records who
answered.

```ts
import type { Turn } from "@helmr/sdk"

export async function requestApproval(turn: Turn) {
  const { answer, answeredBy } = await turn.ask({
    prompt: [{ type: "text", text: "Publish revision 42?" }],
    type: "choice",
    options: [
      { id: "approve", label: "Approve", value: true },
      { id: "decline", label: "Decline", value: false },
    ],
  })
  return { approved: answer.selected[0]?.value === true, by: answeredBy }
}
```

`turn.ask` resolves with `{ answer, answeredBy }`. `answeredBy` is
`{ kind: "user" | "api_key", id }`. Validate the answer before acting on it;
answering a question does not finish the Turn.

## Question types

The prompt accepts a string or Content with `text` and `json` parts. The question's
`type` declares what a valid answer looks like.

| Control | Answer |
| --- | --- |
| `{ type: "text" }` | A string. |
| `{ type: "choice", options, multiple?, allowCustom?, allowNotes? }` | `{ selected: [{ id, value } ], custom?, notes? }` |

`multiple: true` allows more than one selection, and `allowCustom: true` allows
a custom alternative to a single selection, or custom text alongside multiple
selections. `allowNotes` permits supplementary notes; notes alone are not an
answer. Answers that do not match the question are rejected. Option fields, validation rules and size limits are
in the [Questions reference](/docs/reference/sdk/questions).

## Who can answer

A person answers from the CLI or from Slack; code answers through the
client or API.

- **CLI.** `helmr session turn ask list`, `ask get` and `ask answer` find a
  question by its Session and Turn, show its complete question, and submit an
  answer. See [Answer questions](/docs/guides/answer-questions).
- **Slack.** For a Session routed to [Slack](/docs/concepts/slack), the question
  appears in the thread with direct buttons for short choices or an Answer button
  that opens a form. The person
  answering must have linked their Slack identity to Helmr. Each Slack question
  links to authenticated Console inspection and CLI instructions.
- **Client or API.** `turn.asks.answer` on a Turn reference from `HelmrClient`.

Any active member of the Organization can answer. An API key can answer only
if it was issued with permission to respond to questions. Seeing a question,
in Slack or elsewhere, does not by itself grant the right to answer it. The
answer is attributed to the user or API key that submitted it.

Questions can be asked only from a root Session, not from a Session spawned as
another Session's child. See [Delegation](/docs/concepts/delegation).

## Lifecycle

| Status | Meaning |
| --- | --- |
| `pending` | Asked and waiting for an answer. |
| `answered` | Answered; the answer and responder are recorded. |
| `cancelled` | Withdrawn. It can no longer be answered. |

A question is answered at most once. It is withdrawn when:

- the `signal` passed to `turn.ask(question, { signal })` aborts;
- the Turn ends for any reason, including interruption, `maxTurnDuration`,
  cancellation or failure.

An answer is accepted only while its Turn is running. An answer to a withdrawn
question is rejected; it never carries over to a later Turn.

## Retries

Every answer carries a **answer ID** chosen by the answerer. If a response
might not have arrived, retry with the same answer ID and the same answer;
Helmr returns the original result instead of recording a second answer. A
different answer ID, or the same ID with a different answer, is rejected once
the question has been answered.

## Rules for Agent code

- Await every question you ask. A Turn whose work finishes while a question it
  asked is still unresolved fails.
- New questions can be asked only while the handler is processing. A question
  awaited by a message callback that started before the handler returned can
  still be answered while the Turn finalizes.
- `maxTurnDuration` includes time spent waiting for answers. Without it, a Turn
  can wait for a person indefinitely.
- Use a question, not a [message](/docs/concepts/sessions#live-messages), when
  you need an attributable answer. A message never answers a question.
- When a harness asks for permission or input through its own callback, bind
  the answer to that exact native request and propagate cancellation to it.
- Keep credentials out of prompts and answers.

## Related

- [Answer questions](/docs/guides/answer-questions)
- [Questions reference](/docs/reference/sdk/questions)
- [`helmr session`](/docs/reference/cli/session)
