---
title: Questions
description: turn.ask(), answer controls, answer shapes, limits, and the client ask methods.
sidebarLabel: Questions
---

# Questions

This page lists the question API. For how questions behave across
interruption, expiry and authority, see [Questions](/docs/concepts/questions).
To answer one, see [Answer questions](/docs/guides/answer-questions).

## `turn.ask()`

```ts
turn.ask<C extends AnswerControl>(
  question: Question<C>,
  options?: { signal?: AbortSignal },
): Promise<AskResponse<AnswerFor<C>>>
```

Available in root Sessions only. Await every question before the handler
returns; a Turn that returns with an unjoined question fails.

```ts
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
// response.answer: { selected: [{ id: "approve", value: true }] }
// response.respondedBy: { kind: "user", id: "..." }
```

## Types

```ts
type Question<C extends AnswerControl = AnswerControl> = Readonly<{ prompt: Content; answer: C }>

type AnswerControl =
  | Readonly<{ type: "text" }>
  | Readonly<{ type: "choice"; options: readonly ChoiceOption[]; multiple?: boolean; allowText?: boolean }>

type ChoiceOption = Readonly<{ id: string; label: string; description?: string; value: JsonValue }>

type ChoiceAnswer = Readonly<{ selected: readonly Readonly<{ id: string; value: JsonValue }>[]; text?: string }>

type AnswerFor<C extends AnswerControl> = C extends { type: "text" } ? string : ChoiceAnswer

type AskResponse<T = JsonValue> = Readonly<{
  answer: T
  respondedBy: Readonly<{ kind: "user" | "api_key"; id: string }>
}>
```

`prompt` is `Content` (an array; no string shorthand).

## Answer shapes

| Control | Answer |
| --- | --- |
| `{ type: "text" }` | A JSON string. |
| `{ type: "choice", ... }` | `{ selected: [{ id, value }], text? }` |

Choice answer rules:

- Each selected item repeats a declared option's exact `id` and `value`.
- Items appear in declaration order, without duplicates.
- More than one item requires `multiple: true`.
- `text` requires `allowText: true`.
- At least one selected item or nonempty `text` is required.

An answer that does not match its control is rejected with `answer_invalid`.

## Limits

| Item | Limit |
| --- | --- |
| Question (prompt and control) | 256 KiB encoded JSON |
| Prompt | 64 Content parts |
| Choice options | 1–100 |
| Option `id` | 1–128 bytes, unique |
| Option `label` | 256 bytes |
| Option `description` | 2 KiB |
| Answer | 64 KiB |
| Choice custom `text` | 8 KiB |

A choice control is also rejected when its largest possible answer would exceed
64 KiB. Questions count toward the Turn's 8 MiB output budget. Keep credentials
out of question content.

## Client methods

```ts
const turn = client.sessions.ref(sessionId).turns.ref(turnId)

turn.asks.list(query?: { cursor?: string; limit?: number }, options?): Promise<AskPage>
turn.asks.get(askId: string, options?): Promise<AskState>
turn.asks.respond(
  askId: string,
  request: { answer: JsonValue; responseId: string },
  options?,
): Promise<AskResponseReceipt>
```

`AskPage` is `{ items: AskState[], nextCursor? }`; `limit` is 1–100.
`responseId` is required, nonempty, at most 512 bytes. Reuse the same
`responseId` and answer to retry an uncertain response.

| `AskState` field | Type |
| --- | --- |
| `id`, `sessionId`, `turnId` | `string` |
| `status` | `"pending" \| "responded" \| "cancelled"` |
| `prompt` | `Content`, optional |
| `answerControl` | `AnswerControl`, optional |
| `answer` | `JsonValue`, optional |
| `payloadExpired` | `true`, optional |
| `createdAt` | `string` |
| `respondedAt`, `cancelledAt` | `string`, optional |
| `respondedByUserId`, `respondedByApiKeyId` | `string`, optional |

`AskResponseReceipt` is `AskState` without `prompt` and `answerControl`.

An API key needs explicit answer permission to respond; see
[Authentication](/docs/reference/api/authentication). The CLI equivalents are
`helmr session turn ask list|get|respond`; see
[`helmr session`](/docs/reference/cli/session#questions).
