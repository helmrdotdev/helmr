---
title: Actors and Sessions
description: Define Actors and use their durable input and output channels.
---

# Actors and Sessions

`actor({ id, run, idleTimeout?, queue?, maxDuration?, ttl?, retry? })` defines a
long-lived workflow. Starting it creates a server-identified Session and an
initial Run.

```ts
export const reviewer = actor({
  id: "reviewer",
  run: async (session) => {
    const input = await session.input.receive().unwrap()
    await session.output.append({ received: input })
  },
})
```

`actor.start({ workspace, key?, input?, idempotencyKey?, run?, signal? })`
returns `{ session, run }`. The runtime Session has fixed `input` and `output`
channels. `receive({ timeout?, idleTimeout?, metadata?, tags? })` produces a
result or `wait_timeout`/`session_closed`. Output supports `append`, `pipe`, and
`writer`; records have a durable integer sequence and Run provenance.

Outside the guest, use `client.actors.start(declaredId, request)` and address
the returned Session with `client.sessions.ref(id)`. A `SessionRef` provides
`input.send`, `output.list({ after?, limit? })`, `retrieve`, and `close`.
Session statuses are `open`, `closed`, `cancelled`, and `failed`.
