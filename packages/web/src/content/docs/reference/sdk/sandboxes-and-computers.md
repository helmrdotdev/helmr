---
title: Sandboxes and Computers
description: Declare Sandbox capacity and operate durable Computers.
---

# Sandboxes and Computers

A Sandbox is a deployed source declaration; a Computer is a durable resource
created from it.

```ts
export const repo = sandbox({ id: "repo" })
  .image(image("repo").from("node:24-bookworm-slim"))
  .resources({ cpu: 2, memory: "4GiB" })
```

The builder requires `.image(imageBuilder).resources({ cpu, memory })`. Memory is expressed
as `${bigint}MiB` or `${bigint}GiB`. Create externally with
`client.sandboxes.createComputer(declaredId, { key?, idempotencyKey?,
secrets? })`; the result is a `ComputerRef`.

`client.computers.ref(id)` exposes:

| API | Result |
| --- | --- |
| `retrieve()` | Current Computer lifecycle, residency and secret bindings. |
| `members({ cursor?, limit? })` | A page of Sessions, Tasks and Commands attached to the Computer. |
| `exec({ command, idempotencyKey, cwd?, env?, stdin?, timeout? })` | A durable `CommandRef` after admission. |
| `delete({ idempotencyKey? })` | Deletion receipt. |

Computer `status` describes resource lifecycle: `available`, `deleting`, or `deleted`.
Its separate `residency` describes execution: `cold`, `starting`, `running`,
`parking`, `parked`, `restoring`, or `unavailable`. An unavailable Computer includes
an `error` with a code and message. A parked Computer can resume; parking is not a
resource failure. Starting a Task, Actor or Command requests startup or restoration
automatically.
Secrets are bound at creation using plain string names; plaintext secret values are not part of a Computer request.

## Command output and completion

`computer.exec()` acknowledges command admission, including while its Computer is
starting. Reconnect using `client.commands.ref(id)` from your application backend.

| Command API | Result |
| --- | --- |
| `retrieve()` | Command status and an outcome when available. |
| `cancel({ signal? })` | An accepted cancellation receipt with `id`, `targetId` and `status`. |
| `wait({ waitTimeout?, signal? })` | Terminal outcome; a nonzero exit is an ordinary `exited` outcome. |
| `logs({ cursor?, limit? }, { signal? })` | One finite page of retained output and gap records. |
| `streamLogs({ after? }, { signal? })` | Retained output followed by new output until the output closes. |

Command operations are available through the external client. Actor and Task code
uses ordinary child processes for commands on its own Computer, or child Tasks
for work on another Computer. The in-Run `ComputerRef` does not expose `exec()`;
the external client's `ClientComputerRef` does. A local child-process promise is
not a Helmr managed wait.

Each external observer has its own timeout or abort signal. Observing a Command
does not create an Actor or Task.

```ts
const command = await computer.exec({
  command: ["npm", "test"],
  idempotencyKey: "test:revision-42",
})

for await (const record of command.streamLogs({}, { signal })) {
  if (record.kind === "output") {
    const destination = record.stream === "stdout" ? process.stdout : process.stderr
    destination.write(record.content)
  } else {
    console.warn(`Missing ${record.stream} chunks: ${record.fromSequence}–${record.throughSequence}`)
  }
  // Persist after processing to reconnect with streamLogs({ after: record.cursor }).
}
const outcome = await command.wait()
```

Output records contain byte chunks (`Uint8Array`), not necessarily complete text
lines or UTF-8 characters. Each record has an opaque cursor and identifies stdout
or stderr. Ordering is preserved within each stream; timestamps do not establish a
causal order between stdout and stderr.

Finite pages contain `items` and a `nextCursor` when records were returned. Follow
that cursor to request another page; an empty page does not mean a running command
has finished. Streaming handles temporary log-delivery lag without advancing past
undelivered output. The command outcome can be available before logs finish
reaching their read store.

Call `command.cancel()` to request termination. Repeated calls return the same
retained operation receipt; the receipt confirms intent, not process exit. Use
`retrieve()` or `wait()` for the outcome. Pending work that has not been assigned is
cancelled without starting. Assigned work retains its execution identity until the
worker confirms cleanup; `processReconciled` reports that separate fact. Commands
are independent of Actor and Task lifecycles; cancelling a Run does not cancel
a Command on the same Computer.

Breaking iteration or aborting observation does not stop the command.
Log records have bounded retention. Missing ranges are explicit gap records; when complete history or a
failed producer's final output cannot be established, observation reports an error
instead of a successful empty history. Complete-history reads are unavailable once
the Command's creation time crosses the 90-day retention boundary, even if some
newer chunks have not yet expired from storage.
