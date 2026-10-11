---
title: Wait for human input
description: Ask a typed question and respond to its exact Session and Turn.
---

# Wait for human input

In an Agent handler, await an exact question:

```ts
import type { Turn } from "@helmr/sdk"

export async function chooseRevision(turn: Turn) {
  const { answer, respondedBy } = await turn.ask({
    prompt: [{ type: "text", text: "Which revision should I review?" }],
    answer: { type: "text" },
  })
  return { revision: answer, responder: respondedBy }
}
```

The handler can then validate and use the answer. Finishing the question does
not finish the Turn. Complete native work and application checks before returning
its machine result or staging a response.

Find and answer the question from an authenticated CLI:

```sh
helmr session turn ask list --session SESSION_ID --turn TURN_ID --project agents --env development --json
helmr session turn ask get ASK_ID --session SESSION_ID --turn TURN_ID --project agents --env development --json
helmr session turn ask respond ASK_ID --session SESSION_ID --turn TURN_ID \
  --project agents --env development \
  --answer-json '"revision-42"' --response-id answer:revision-42 --json
```

Use the displayed control to construct the answer. Keep the same response ID
and answer across an uncertain retry. Current membership or explicit API-key
answer permission is required. Answers are attributed to that principal.

Use `turn.onMessage` for free-form steering and exact Turn messages for live
corrections. Use the ask response API for questions; a message does not settle an
ask. Native permission callbacks must bind the answer to their live native request
and observe stop/convergence rules. See [Questions](/docs/reference/sdk/questions).
