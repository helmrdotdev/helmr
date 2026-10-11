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
| After a Turn's handler returns | The Computer's disk. The Turn becomes `completed` only after this save is stored durably. |
| Periodically while the Computer runs | The Computer's disk, on an interval the operator configures. This can happen in the middle of a Turn. |
| When an idle Session hibernates | The Computer's memory, processes and disk. |

A Turn that returns moves to `finalizing` while its save is written, then to
`completed`. If the save cannot be stored, the Turn ends `interrupted` instead.
A Turn that throws ends `failed` without a save of its own; its disk changes
are kept on the running Computer and saved by the next save.

A [Command](/docs/concepts/computers#commands) does not trigger a save when it
finishes. Its changes are saved by the next periodic save, Turn save or
hibernation.

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
  computer: repoComputer,
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

If the worker is lost while the Computer is running, the running Turn ends
`interrupted` and the Session is held, as with process loss. The Computer
reports `unavailable` residency with the error code
`computer_recovery_required`, and Sessions on it cannot run again: this release
has no operation that brings such a Computer back. Start new work on a new
Computer.

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
