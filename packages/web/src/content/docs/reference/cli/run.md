---
title: helmr run
description: List, inspect, follow, wait for, and cancel Runs.
sidebarLabel: run
---

# `helmr run`

```text
helmr run list [--status STATUS] [--cursor C] [--limit N] [--json | --jsonl]
helmr run get RUN [--json]
helmr run logs RUN [--follow | --wait-ready DURATION]
helmr run events RUN [--cursor C] [--limit N] [--follow | --wait-ready DURATION]
helmr run wait RUN [--timeout DURATION] [--json]
helmr run cancel RUN [--json]
```

All commands accept project/environment scope. `--status` may be repeated or
comma-separated. Log and event follow modes poll finite pages until terminal
Run state; cursors are opaque. `events` writes JSON event records. `wait` polls
the Run resource and optionally bounds the wait. For finite log or event reads,
`--wait-ready` retries only while terminal telemetry replay is still catching up;
other read failures are returned immediately.
