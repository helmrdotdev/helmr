---
title: Computers
description: Inspect Computer definitions and operate durable Computers.
---

# Computers

A Computer definition describes a deployed environment. A Computer is a durable
instance created from that definition.

```ts
export const repo = computer({
  id: "repo",
  image: image("repo").from("node:24-bookworm-slim"),
  resources: { cpu: 2, memory: "4GiB" },
})
```

`client.computerDefinitions.list({ deploymentId?, cursor?, limit? })` lists
identities in the selected Deployment, or the current Deployment when omitted.
The page includes `deploymentId`, `items` and an optional `nextCursor`; continuation
keeps the selected Deployment. Use `.retrieve(id, { deploymentId? })` for one
identity and its owning Deployment. Limits range from 1 to 100.

Create an actual Computer with
`client.computerDefinitions.createComputer(id, { key?, idempotencyKey?, secrets? })`.
Creation uses the current Deployment and returns a `ClientComputerRef`.
Its snapshot exposes the source `definitionKey` and `deploymentId`.

`client.computers.ref(id)` exposes:

| API | Result |
| --- | --- |
| `retrieve()` | Current Computer lifecycle, residency and secret bindings. |
| `members({ cursor?, limit? })` | A page of Sessions and Commands attached to the Computer. |
| `exec({ command, idempotencyKey, cwd?, env?, stdin?, timeout? })` | A durable `CommandRef` after admission. |
| `delete({ idempotencyKey? })` | Deletion receipt. |

Computer `status` describes resource lifecycle: `available`, `deleting`, or `deleted`.
Its separate `residency` describes execution: `cold`, `starting`, `running`,
`parking`, `parked`, `restoring`, or `unavailable`. An unavailable Computer includes
an `error` with a code and message. A parked Computer can resume; parking is not a
resource failure. Starting an Agent or Command requests startup or restoration
automatically.
Secrets are bound at creation using stable `secretId` values and explicit env or
file placements. Protected env bindings use `allowedOrigins`. Plaintext values
are not part of a Computer request.

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

Command operations use the explicitly authenticated `HelmrClient`, including
when that client is constructed inside Agent code. Its credentials determine
its authority; the current Session does not grant or restrict those client calls.
The server still checks permissions, Computer availability and execution limits.
Injected runtime Computer references do not expose `exec()`; use ordinary child
processes for local work or an explicit client for authorized Computer commands.

Each external observer has its own timeout or abort signal. Observing a Command
does not create an Agent Session.

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
are independent of Session lifecycles; cancelling a Session does not cancel
a Command on the same Computer.

Breaking iteration or aborting observation does not stop the command.
Log records have bounded retention. Missing ranges are explicit gap records; when complete history or a
failed producer's final output cannot be established, observation reports an error
instead of a successful empty history. Complete-history reads are unavailable once
the Command's creation time crosses the 90-day retention boundary, even if some
newer chunks have not yet expired from storage.
