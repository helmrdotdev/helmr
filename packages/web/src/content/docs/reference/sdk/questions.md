---
title: Questions
description: turn.ask(), question types, answer shapes, limits, and the client ask methods.
sidebarLabel: Questions
---

# Questions

This page lists the question API. For how questions behave across
interruption, expiry and authority, see [Questions](/docs/concepts/questions).
To answer one, see [Answer questions](/docs/guides/answer-questions).

## `turn.ask()`

```ts
turn.ask<const Q extends Question>(
  question: Q,
  options?: { signal?: AbortSignal },
): Promise<AskAnswer<AnswerFor<Q>>>
```

Available in root Sessions only. Await every question before the handler
returns; a Turn that returns with an unjoined question fails.

```ts
const response = await turn.ask({
  prompt: [{ type: "text", text: "Publish this revision?" }],
  type: "choice",
  options: [
    { id: "approve", label: "Approve", value: true },
    { id: "decline", label: "Decline", value: false },
  ],
})
// response.answer: { selected: [{ id: "approve", value: true }] }
// response.answeredBy: { kind: "user", id: "..." }
```

## `validateQuestion()`

```ts
import { ContentError, validateQuestion } from "@helmr/sdk"

// Accepts unknown input and narrows it to Question on success.
validateQuestion(value: unknown): asserts value is Question
```

Checks the same definition rules and limits as `turn.ask` without modifying the
input, storing a question, or displaying it. A string prompt remains a string.
Success returns `undefined`; failure throws `ContentError` with a `code` and
human-readable `message`. Codes are `invalid_arguments`,
`content_kind_unsupported`, or `content_limit_exceeded`. Use the code for program
logic; messages explain the failure and are not stable identifiers.

To check several generated questions before displaying the first, validate all
of them before calling `turn.ask`:

```ts
for (const question of questions) validateQuestion(question)
for (const question of questions) {
  const response = await turn.ask(question)
  // Handle each answer.
}
```

Validation checks the definition at the time of the call. `turn.ask` validates
again when called. Validation does not reserve output budget, check Session
state or authority, or guarantee admission or all-or-none success of several
questions. Handle execution failures and cancellation normally.

## Types

```ts
type Question = TextQuestion | ChoiceQuestion
// Both variants accept prompt: HumanContent and optional title: string.
// TextQuestion has type: "text".
// ChoiceQuestion has type: "choice", options, and optional
// multiple, allowCustom, allowNotes (all false by default).

type AnswerSubmission = string | Readonly<{
  selectedIds: readonly string[]
  custom?: string
  notes?: string
}>
```

`prompt` accepts a string or a Content array. Read models retain normalized
Content arrays. Text questions forbid choice-only fields.

## Answer shapes

Text questions resolve to a string. Choice questions resolve to
`{ selected: [{ id, value }], custom?, notes? }`. The inferred selection preserves
each declared ID's associated value type.

Remote callers submit `selectedIds`; the server resolves values from the
immutable question and returns selections in declaration order. Duplicate or
unknown IDs are rejected. Single choice requires exactly one selection, or an
allowed nonempty custom answer with no selection. Multiple choice requires at
least one selection or allowed custom answer. `custom` requires `allowCustom`;
`notes` requires `allowNotes`. Notes supplement an answer and cannot satisfy the
question alone. Both text fields must be nonempty when supplied.

`turn.ask` returns `{ answer, answeredBy }`, where `answeredBy` identifies the
verified user or API key. Invalid submissions return `answer_invalid`.

## Limits

| Item | Limit |
| --- | --- |
| Question | 256 KiB encoded JSON |
| Optional `title` | Nonblank, at most 256 UTF-8 bytes |
| Prompt | 64 Content parts |
| Choice options | 1–100 |
| Option `id` | 1–128 bytes, unique |
| Option `label` | 256 bytes |
| Option `description` | 2 KiB |
| Answer | 64 KiB |
| Choice `custom` or `notes` | 8 KiB |

A choice question is also rejected when its largest possible answer would exceed
64 KiB. Questions count toward the Turn's 8 MiB output budget. Keep credentials
out of question content.

## Client methods

```ts
const turn = client.sessions.ref(sessionId).turns.ref(turnId)

turn.asks.list(query?: { cursor?: string; limit?: number }, options?): Promise<AskPage>
turn.asks.get(askId: string, options?): Promise<AskState>
turn.asks.answer(
  askId: string,
  request: { answer: AnswerSubmission; answerId: string },
  options?,
): Promise<AskAnswerReceipt>
```

`AskPage` is `{ items: AskState[], nextCursor? }`; `limit` is 1–100.
`answerId` is required, nonempty, at most 512 bytes. Reuse the same
`answerId` and answer to retry an uncertain response.

| `AskState` field | Type |
| --- | --- |
| `id`, `sessionId`, `turnId` | `string` |
| `status` | `"pending" \| "answered" \| "cancelled"` |
| `question` | `NormalizedQuestion`, optional |
| `answer` | `JsonValue`, optional |
| `payloadExpired` | `true`, optional |
| `createdAt` | `string` |
| `answeredAt`, `cancelledAt` | `string`, optional |
| `answeredByUserId`, `answeredByApiKeyId` | `string`, optional |

`AskAnswerReceipt` is `AskState` without `question`.

An API key needs current Role access to answer in the target Session; see
[Authentication](/docs/reference/api/authentication). The CLI equivalents are
`helmr session turn ask list|get|answer`; see
[`helmr session`](/docs/reference/cli/session#questions).
