---
title: helmr actor
description: Start Actors and operate their Sessions.
sidebarLabel: actor
---

# `helmr actor`

```text
helmr actor start ACTOR --workspace WORKSPACE [flags]
helmr actor get SESSION_ID [--json]
helmr actor input send SESSION_ID (--input-json JSON | --input-file FILE) [--json]
helmr actor output read SESSION_ID [--after N] [--limit N] [--json | --jsonl]
helmr actor close SESSION_ID [--idempotency-key KEY] [--json]
```

`start` accepts `--key`, initial input, `--idempotency-key`, and managed-Run
options: queue, concurrency key, priority, TTL, tags, metadata, and retry
policy. It always requires an existing Workspace.

Commands after start address the server-created Session ID, not the Actor
declaration ID. Input is one JSON value. Output is one finite durable sequence
page; `--after` is an integer and `--limit` has a maximum of 100.
