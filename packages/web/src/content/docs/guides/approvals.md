---
title: Human input
description: Pause tasks for operator decisions or message input.
section: Guides
sidebarLabel: Human input
order: 330
---

# Human input

Use waitpoints before side effects such as posting to GitHub, deploying, or changing infrastructure.

```ts
const decision = await ctx.wait.token<{ approved: boolean }>({
  displayText: "Post this review summary?",
})
if (!decision.approved) {
  return { status: "skipped" }
}
```

Ask for operator input with another token waitpoint:

```ts
import { writeFile } from "node:fs/promises"

const reply = await ctx.wait.token<{ text: string }>({
  displayText: "What should this run write to handoff.txt?",
})
await writeFile("handoff.txt", `${reply.text}\n`)
```

Token waitpoints accept a timeout in seconds:

```ts
await ctx.wait.token({ displayText: "Continue?", timeout: 600 })
```

Resolve waitpoints from the dashboard or CLI:

```sh
helmr resume complete RUN_ID WAITPOINT_ID --value '{"approved":true}'
helmr resume complete RUN_ID WAITPOINT_ID --value '{"text":"Use the smaller rollout."}'
```

Only one `ctx.wait.*` call can be active at a time in a task. Await each waitpoint before starting the next one.
