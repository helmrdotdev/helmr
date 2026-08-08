---
title: Timers
description: Durable time waits inside a Helmr Run.
---

# Timers

`timers.waitFor(duration)` waits for a positive Helmr duration and
`timers.waitUntil(date)` waits until an absolute JavaScript `Date`.

```ts
await timers.waitFor("30s")
await timers.waitUntil(new Date("2026-08-10T09:00:00Z"))
```

Both APIs return `Promise<void>` and are guest-runtime operations. Await them so
the Run can durably park and resume. Durations use `ms`, `s`, `m`, `h`, or `d`.
