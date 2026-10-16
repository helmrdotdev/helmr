---
title: Durability
description: What Helmr saves for a Session, when it saves it, and what happens when a process or host is lost.
---

# Durability

Helmr keeps an Agent's work across Turns, idle periods and restarts without any
save or restore code in the Agent. Your `init` result, running harness processes
and files on the Computer carry over from one Turn to the next. This page
describes exactly what is saved, when, and where the guarantees stop.

## What you do not write

There is no checkpoint API and no state serialization step. Helmr saves the
Computer's disk and, when a Session hibernates, its memory and processes. An
Agent therefore:

- keeps live objects in `initResult`, such as clients, child processes and
  open harness sessions, without converting them to JSON;
- writes files to the Computer's disk as ordinary files;
- does not register save or restore hooks.

What an Agent does need is to make external effects safe to repeat, because an
interrupted Turn is never replayed for it but may be retried by a person. See
[Reconstruction](#reconstruction).

## When the disk is saved

| Moment | What is saved |
| --- | --- |
| After the Computer's files change | A background save is scheduled after a short quiet period, with a maximum scheduling wait while changes continue. This also runs during a Turn or Command. |
| After a Turn completes | Its completed result is tracked until a background save covers its file changes. The reply does not wait for saving. |
| When an idle Session hibernates | The Computer's memory, processes and disk. |

A completed Turn retains its result and response independently of disk saves.
Helmr retries a failed save while work continues, as long as local staging has
room. A save failure does not change a completed Turn to interrupted. A Turn
that throws ends `failed` without a save of its own; its disk changes remain on
the running Computer and can be included in a later save.

If local storage fills, new requests wait while Helmr keeps saving. While the
execution environment remains healthy and authorized, a continuous storage pause
is shown after 30 seconds and a running request is stopped after five minutes.
Blocking I/O can cause execution to be lost earlier; those cases follow the
process or Computer recovery rules below. Stopped requests are not replayed.

Cooperative stops retain their normal time limits while storage is blocked.
A process that cannot stop within its limit requires reconstruction and
acknowledgement. Local changes remain available only while that environment is
retained; recovery can omit changes that have not been saved. A manual Resume
does not bypass a storage pause. When storage clears on a healthy environment,
queued work can continue automatically.

A [Command](/docs/concepts/computers#commands) does not trigger a separate save
when it finishes. Its file changes use the same background autosave. Autosave
does not periodically flush idle Computers. A newly completed Turn still needs
a save receipt even when it made no file changes.

Saves are stored encrypted. Each save replaces the previous one as the
Computer's current state; there is no history of earlier saves to roll back to.

## Hibernation

When a Computer has no running or queued Turns, no pending Commands and no save
in progress, Helmr hibernates it. Hibernation captures memory, running processes
and disk, then releases the worker. There is no separate idle timeout for
hibernation, and it does not close the Session. A Turn waiting for the answer to a
[question](/docs/concepts/questions) is still running, so its Computer keeps
running during the wait.

New input to any Session on the Computer, or a new Command, restores it. The
Session resumes exactly where it stopped: processes are still running, memory
is intact and `init` does not run again. A hibernated Computer reports
`cold` residency, and `restoring` while it resumes. It can resume on a
different worker in the same worker group.

Hibernation is separate from `closeAfterIdle`, which closes the Session. See
[Agents](/docs/concepts/agents#session-lifetime-and-turn-limits).

## Process loss

If the Session's process exits unexpectedly while a Turn runs:

- the running Turn ends `interrupted`, and its pending questions and messages
  are cancelled or rejected;
- the Session is held, so nothing else runs until someone resumes it;
- queued inputs stay queued.

Helmr never replays the interrupted input automatically. Re-send it, or send a
new input, when you decide it is safe.

## Reconstruction

Reconstruction is explicit. Inspect the Session's holds and resume the one
placed by the failure:

```sh
helmr session resume SESSION_ID --hold HOLD_ID
```

In TypeScript, call `session.resume({ holdId })` on a session reference. When
the Session next has input to run, Helmr starts a new process on the same
Computer and runs `init` again. In that process `ctx.recovery.kind` is
`"reconstructed"` and `ctx.recovery.reason` is `"process_replaced"`; in a
Session's first process it is `"initial"`.

Reconstruction keeps the Computer's disk as the lost process left it,
including changes the interrupted Turn made. It does not restore memory, the
old process tree or output the interrupted Turn had not committed. Use
`ctx.recovery` in `init` to reconnect to your harness and reconcile external
work:

```ts
// openHarness and reconcilePendingWork stand for your own harness code.
export const coder = agent({
  id: "coder",
  template: repoComputer,
  async init(ctx) {
    const harness = await openHarness("/workspace/repo")
    if (ctx.recovery.kind === "reconstructed") {
      await harness.reconcilePendingWork()
    }
    return { harness }
  },
  async turn(turn, { initResult }) {
    return initResult.harness.run(turn.input)
  },
})
```

## Worker loss

A Computer that is hibernated when its worker is lost keeps its saved memory,
processes and disk and resumes normally.

If the worker is lost while the Computer is running, Helmr physically fences
the old writer before restoring the latest eligible saved disk into a new
allocation. It starts new Session processes, so `init` runs again; memory and
live process state are not reconstructed from a disk save. If no eligible
save has ever been published, Helmr can use that Computer's retained initial
work environment. An unavailable newer save is not replaced with an older or
initial image.

If completed requests were not covered by the restored save, Helmr holds every
attached conversation and shows the restore point and the number of affected
completed requests in that conversation. Their file changes and native
conversation history may be missing; their committed results and replies
remain available. Choose Resume in Console, resume the exact hold through the
SDK or CLI, or send a new Slack mention after the recovery notice. This
acknowledges only the recovery hold for that conversation; other holds remain.
Completed handlers never run again automatically.

When no completed work is missing, restoration needs no recovery-gap
acknowledgement and queued requests continue automatically. A request that was
still running ends as interrupted. Its Slack thread receives one message asking
you to mention the agent again; Console shows the interrupted outcome. If a
recovery-gap notice already explains that loss, Helmr does not send a second
interruption notice. Interrupted work is never replayed. If the retained disk or
its encryption keys are unavailable, the Computer remains unavailable rather
than silently using a different environment.

## Limits

- The Computer disk is 32 GiB.
- `initResult` is held in memory. It survives hibernation but not
  reconstruction, so keep anything that must outlive a process on disk or in an
  external system.
- Secret values already delivered to a Computer may be present in its saved
  memory and disk. See [Secrets](/docs/concepts/secrets).

## Related

- [Sessions](/docs/concepts/sessions) for holds and controls
- [Turns](/docs/concepts/turns) for Turn outcomes
- [Computers](/docs/concepts/computers) for residency and Commands
- [Build a durable agent](/docs/guides/build-a-durable-agent)
