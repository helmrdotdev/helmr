---
title: helmr actor
description: Start a Session from a deployed Actor.
sidebarLabel: actor
---

# `helmr actor`

```text
helmr actor start ACTOR --computer COMPUTER [flags]
```

`start` accepts an Actor declaration with project and environment scope flags,
`--key`, `--idempotency-key`, and managed-Run options: queue, concurrency key,
priority, TTL, tags, metadata, and retry policy. It requires an existing Computer
and returns a Session ID and boot Run ID. `--json` prints one JSON object.

Send the first input separately after starting the Actor. Use
[`helmr session`](../session/) to send work, read output, and control that Session.
Reuse the idempotency key and exact arguments when retrying an uncertain start.
