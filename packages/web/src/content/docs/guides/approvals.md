---
title: Approvals
description: Pause tasks for operator approval or message input.
section: Guides
sidebarLabel: Approvals
order: 330
---

# Approvals

Use waitpoints before side effects such as posting to GitHub, deploying, or changing infrastructure.

```ts
const decision = await ctx.wait.approval("Post this review summary?")
if (!decision.approved) {
  return { status: "skipped", approvedBy: decision.approvedBy }
}
```

Ask for operator input with a message waitpoint:

```ts
const reply = await ctx.wait.message("What should this run write to handoff.txt?")
await Bun.write("handoff.txt", `${reply.text}\n`)
```

Both waitpoint types accept a timeout in seconds:

```ts
await ctx.wait.approval("Continue?", { timeout: 600 })
```

Resolve waitpoints from the dashboard or CLI:

```sh
helmr resume approve RUN_ID WAITPOINT_ID --reason "reviewed"
helmr resume deny RUN_ID WAITPOINT_ID --reason "needs changes"
helmr resume message RUN_ID WAITPOINT_ID --text "Use the smaller rollout."
```

Only one `ctx.wait.*` call can be active at a time in a task. Await each approval or message before starting the next waitpoint.
