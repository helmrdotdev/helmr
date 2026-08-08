---
title: helmr task
description: Inspect deployed Tasks and start Task Runs.
sidebarLabel: task
---

# `helmr task`

```text
helmr task list [-p PROJECT] [-e ENV] [--json]
helmr task get TASK [-p PROJECT] [-e ENV] [--json]
helmr task start TASK --workspace WORKSPACE [flags]
```

Start payload sources are mutually exclusive: `--payload-file FILE`,
`--payload-json JSON`, or repeated `--payload KEY=VALUE`. Run options are
`--queue`, `--concurrency-key`, `--priority`, `--ttl`, repeated `--tag`, and
metadata/retry JSON from `--metadata-file`/`--metadata-json` and
`--retry-file`/`--retry-json`.

`--idempotency-key` makes start retries stable. `--wait` waits for terminal
state; `--follow` streams logs while waiting. `--timeout` bounds that wait and
`--json` emits one JSON result. An existing `--workspace` is required.
