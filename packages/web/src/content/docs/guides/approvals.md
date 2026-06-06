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
const decision = await ctx.wait.human<{ approved: boolean }>({
  displayText: "Post this review summary?",
})
if (!decision.approved) {
  return { status: "skipped" }
}
```

Ask for operator input with another human waitpoint:

```ts
import { writeFile } from "node:fs/promises"

const reply = await ctx.wait.human<{ text: string }>({
  displayText: "What should this run write to handoff.txt?",
})
await writeFile("handoff.txt", `${reply.text}\n`)
```

Human waitpoints accept a timeout in seconds:

```ts
await ctx.wait.human({ displayText: "Continue?", timeout: 600 })
```

Resolve waitpoints from the dashboard or CLI:

```sh
helmr waitpoint list
helmr waitpoint respond WAITPOINT_ID --value '{"approved":true}'
helmr waitpoint respond WAITPOINT_ID --value '{"text":"Use the smaller rollout."}'
```

Only one `ctx.wait.*` call can be active at a time in a task. Await each waitpoint before starting the next one.
