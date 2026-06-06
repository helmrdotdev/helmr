---
title: Waitpoints
description: Generic pauses inside a running task.
section: Concepts
sidebarLabel: Waitpoints
order: 170
---

# Waitpoints

Waitpoints pause a run while it waits for time to pass or for an external response. Human-in-the-loop flows use human waitpoints with typed JSON values.

```ts
const decision = await ctx.wait.human<{ approved: boolean }>({
  displayText: "Post this review to GitHub?",
})

if (decision.approved) {
  await postReview()
}

const reply = await ctx.wait.human<{ text: string }>({
  displayText: "What should the task change next?",
})
ctx.log.info(reply.text)
```

## Human Waits

Human waitpoints resolve with the JSON `value` supplied by the dashboard, CLI, API, or delegated response token. Shape the value with TypeScript generics or a schema.

```ts
const input = await ctx.wait.human<{ rollout: "small" | "full" }>({
  displayText: "Choose rollout size",
  timeout: 600,
})
```

CLI responses use one command:

```sh
helmr waitpoint respond WAITPOINT_ID --value '{"rollout":"small"}'
```

Use `helmr waitpoint list` to find open waitpoints.

## Delay Waits

Use delay waitpoints when a task should resume after a duration or timestamp:

```ts
await ctx.wait.for("10m")
await ctx.wait.until(new Date("2026-06-01T00:00:00Z"))
```

## Checkpoints

When a worker creates a waitpoint, Helmr also creates a checkpoint record. Workers use that checkpoint data when continuing from the resolved waitpoint.

Only one `ctx.wait.*` call can be active in an execution at a time.
