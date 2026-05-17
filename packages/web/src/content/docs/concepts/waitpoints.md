---
title: Waitpoints
description: Approval and message pauses inside a running task.
section: Concepts
sidebarLabel: Waitpoints
order: 170
---

# Waitpoints

Waitpoints pause a run while it waits for an operator response. Helmr supports approval waitpoints and message waitpoints.

```ts
const decision = await ctx.wait.approval("Post this review to GitHub?")

if (decision.approved) {
  await postReview()
}

const reply = await ctx.wait.message("What should the task change next?")
ctx.log.info(reply.text)
```

## Approval

An approval waitpoint returns `{ approved, approvedBy, at }`. Operators can approve or deny from the web UI, CLI, or API. CLI commands are:

```sh
helmr resume approve RUN_ID WAITPOINT_ID --reason "looks good"
helmr resume deny RUN_ID WAITPOINT_ID --reason "needs edits"
```

## Message

A message waitpoint returns `{ text, sentBy, at, attachments }`. CLI replies use:

```sh
helmr resume message RUN_ID WAITPOINT_ID --text "Use the smaller change."
```

## Checkpoints

When a worker creates a waitpoint, Helmr also creates a checkpoint record. Workers use that checkpoint data when continuing from the resolved waitpoint.

Only one open waitpoint is allowed per run.
