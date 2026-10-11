---
title: helmr command
description: Inspect, observe, and cancel admitted Computer commands.
sidebarLabel: command
---

# `helmr command`

```text
helmr command get COMMAND_ID [--json]
helmr command wait COMMAND_ID --timeout DURATION [--json]
helmr command logs COMMAND_ID [--cursor CURSOR] [--limit N] [--follow] [--json]
helmr command cancel COMMAND_ID [--json]
```

Use the Command ID returned by `helmr computer exec`. All operations accept `--project` and `--env`; otherwise they use the active scope.

`wait` observes until settlement or its deadline. JSON is `{ "status": "settled", "outcome": ... }` or `{ "status": "timeout" }`. A nonzero exit remains an `exited` outcome. Timeout does not cancel execution. The duration is a positive integer followed by `ms`, `s`, `m`, `h`, or `d`; compound and fractional values are rejected.

`logs` reads one finite page. Text output preserves stdout and stderr bytes and reports gaps, output state, and the next cursor. JSON retains base64 byte records, `output_state`, and `next_cursor`. `--follow` continues after retained pages until output closes; JSON emits one page per line. An unavailable final output state causes following to fail rather than reporting complete history. Abort stops observation only.

`cancel` returns an accepted receipt. Inspect `get` or `wait` to learn the eventual outcome; acceptance alone does not establish process termination. `process_reconciled` reports cleanup separately from the outcome.
