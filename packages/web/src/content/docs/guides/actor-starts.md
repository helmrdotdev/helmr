---
title: Actor starts
description: Start a stable Actor and continue it through fixed durable channels.
section: Guides
sidebarLabel: Actor starts
order: 370
---

# Actor starts

Use an Actor for one stable identity with many managed Runs over time. The
Actor declaration binds its implementation; callers supply an optional Session
key and receive the Session UUID, one explicit Workspace, and optional initial
input.

```ts
const workspace = await reviewerSandbox.createWorkspace({
  key: "github:OWNER/REPO#42",
})
const started = await reviewer.start({
  key: "github:OWNER/REPO#42",
  workspace,
  input: { type: "review", owner: "OWNER", repo: "REPO", prNumber: 42 },
  idempotencyKey: "github:OWNER/REPO#42:start",
})
```

The optional initial value is sequence 1 of the ordinary Actor input log. It is
not a separate handler payload. A matching live start claim replays the same
Actor and boot Run; an occupied key from a different request conflicts.

Actor code consumes input and publishes progressive durable output:

```ts
export const reviewer = actor({
  id: "reviewer",
  async run(session, ctx) {
    console.log(ctx.actor.id, session.id, session.key)
    const input = await session.input.receive({ idleTimeout: "30m" }).unwrap()
    await session.output.append({ type: "progress", message: "Reviewing", input })
  },
})
```

The returned `started.session` identifies the durable runtime resource. Later
messages use that Session's fixed input channel:

```ts
const ref = sessions.ref(started.session.id)
await ref.input.send(
  { type: "steer", instruction: "Please also update the tests." },
  { idempotencyKey: "slack:T123:C456:1712345678.000100" },
)

for (const record of await ref.output.list()) {
  console.log(record.sequence, record.data)
}
```

Use a direct Task Run for bounded one-shot work. Its durable application output
is the terminal result; it has logs and events but no named input/output
streams.

For an email link or one browser approval button, use a Token instead of
granting channel access. Actor-channel public browser grants are deferred in
v0; `token.complete` is the retained narrow public capability.
