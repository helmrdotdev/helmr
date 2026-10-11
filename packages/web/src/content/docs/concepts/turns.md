---
title: Turns
description: One admitted input, visible output and a terminal outcome after finalization.
---

# Turns

A Turn is one JSON input admitted to a Session. Its ID and sequence identify
that work independently of the native harness. Only one Turn executes at a time
within a Session.

| Status | Meaning |
| --- | --- |
| `queued` | Accepted and waiting for dispatch. |
| `running` | Authored processing is active. |
| `finalizing` | Processing returned; required drainage or durable Save publication is pending. |
| `completed` | Required work and the adequate disk Save committed successfully. |
| `failed` | Application or contract failure. |
| `interrupted` | Interruption, deadline or execution/finalization loss prevented completion. |
| `cancelled` | Session cancellation cancelled this work. |

`turn.output.write` records human-facing content during processing. `turn.respond`
stages an optional human answer for successful settlement. The handler's return
value is a separate machine result. Native final text, output EOF and a handler
return do not by themselves establish Completed.

Finalization drains already-started callbacks and output, converges native work
and publishes an adequate Computer disk cut. Applications finish writes and flush
their own buffers before returning. Routine publication retries retain the same
finalization identity and do not rerun the handler. Existing output remains
readable while finalization is pending.

Computer Saves are crash-consistent Computer-wide state. On a shared Computer,
a Save can include a peer's partial writes. They do not provide per-Turn rollback,
immutable historical files or exactly-once external effects. A result containing
a path is not a file download or a backup of its bytes.

Observe outcomes using `turn.wait()` or retained Turn reads. A bounded wait can
return timeout; that stops observation only. Failed, interrupted and cancelled
are terminal outcomes too. Retention expiry is explicit in reads.

Safely converged application failure keeps the Session open. Runtime loss can hold
later work for explicit recovery. Resume does not replay the interrupted Turn;
business retries and external-effect reconciliation belong to the application.
