---
title: Runtime client
description: TypeScript client APIs for starting tasks, opening workspaces, and inspecting sessions and runs.
section: Reference
sidebarLabel: Runtime client
order: 920
---

# Runtime client

`HelmrClient` provides the TypeScript runtime API:

```ts
import { HelmrClient } from "@helmr/sdk"

const client = new HelmrClient({
  url: process.env.HELMR_API_URL,
  apiKey: process.env.HELMR_API_KEY,
})
```

Authenticated SDK calls require an API key. Scoped public access tokens are
limited to one Token completion authority. `http://` is allowed only for
loopback hosts.

The SDK sends a pinned `Helmr-API-Version` header and `Helmr-SDK-Version` on every request. The pinned API version is the contract the SDK was built and tested against; it does not change with the current date.

Main surfaces:

| API | Purpose |
| --- | --- |
| `sessions.start(taskObject, payload, opts)` / `client.sessions.start(taskObject, payload, opts)` | Start or reuse a session and return the session plus first/current run handle. Pass a task object for local payload validation and type inference; pass a string id for external boundaries or dynamic task ids. |
| `sessions.startAndWait(taskObject, payload, opts)` / `client.sessions.startAndWait(taskObject, payload, opts)` | Start or reuse a session, then wait for the first run to become terminal or time out. The session remains open unless explicitly closed or cancelled. Pass a task object for local payload validation and type inference; pass a string id for external boundaries or dynamic task ids. |
| `sessions.retrieve(session)` / `client.sessions.retrieve(session)` | Fetch current session state. |
| `actor.ref({ id | key }).input.send(value)` | Append one durable value to an Actor's fixed input channel. |
| `actor.ref({ id | key }).output.list(opts)` | Read durable Actor output records from a cursor. |
| `actor.ref({ id | key }).output.read(opts)` | Iterate durable Actor output records from a cursor. |
| `workspaces.create(opts)` / `client.workspaces.create(opts)` | Create a durable workspace from a deployed sandbox. |
| `workspaces.open(id)` / `client.workspaces.open(id)` | Create a lazy handle for a workspace. |
| `workspaces.retrieve(idOrHandle, opts)` / `client.workspaces.retrieve(idOrHandle, opts)` | Fetch current workspace state. |
| `workspaces.list(opts)` / `client.workspaces.list(opts)` | List workspaces in the selected project environment. |
| `workspaces.update(idOrHandle, opts)` / `client.workspaces.update(idOrHandle, opts)` | Update workspace metadata or tags. |
| `workspaces.delete(idOrHandle, opts)` / `client.workspaces.delete(idOrHandle, opts)` | Delete a workspace. |
| `workspaces.materialize(idOrHandle, opts)` / `client.workspaces.materialize(idOrHandle, opts)` | Request worker materialization for a workspace. |
| `workspaces.connect(idOrHandle, opts)` / `client.workspaces.connect(idOrHandle, opts)` | Connect to an existing or newly materialized workspace. |
| `workspaces.stop(idOrHandle, opts)` / `client.workspaces.stop(idOrHandle, opts)` | Stop the active materialization for a workspace. |
| `workspaces.open(id).files.read(path, opts)` / `client.workspaces.open(id).files.read(path, opts)` | Read raw bytes from a ready workspace version. |
| `workspaces.open(id).files.list(path, opts)` / `client.workspaces.open(id).files.list(path, opts)` | List direct children from a ready workspace version. |
| `workspaces.open(id).files.stat(path, opts)` / `client.workspaces.open(id).files.stat(path, opts)` | Read metadata for one file, directory, or symlink in a ready workspace version. |
| `workspaces.open(id).versions.retrieve(versionId, opts)` / `client.workspaces.open(id).versions.retrieve(versionId, opts)` | Retrieve one ready workspace version. |
| `workspaces.open(id).versions.list(opts)` / `client.workspaces.open(id).versions.list(opts)` | List ready workspace versions. |
| `workspaces.open(id).exec(command, opts)` / `client.workspaces.open(id).exec(command, opts)` | Start a write-capable command in the workspace. |
| `workspaces.open(id).execs.list(opts)` / `client.workspaces.open(id).execs.list(opts)` | List execs for a workspace. |
| `workspaces.open(id).pty.create(opts)` / `client.workspaces.open(id).pty.create(opts)` | Start an interactive PTY in the workspace. |
| `workspaces.open(id).pty.list(opts)` / `client.workspaces.open(id).pty.list(opts)` | List PTYs for a workspace. |
| `runs.retrieve(run)` / `client.runs.retrieve(run)` | Fetch current run snapshot. |
| `runs.wait(run, opts)` / `client.runs.wait(run, opts)` | Wait for terminal status using durable run events. |
| `runs.list(opts)` / `client.runs.list(opts)` | List run summaries. |
| `runs.logs.retrieve(run)` / `client.runs.logs.retrieve(run)` | Read latest stdout/stderr snapshot. |
| `runs.events.list(run, opts)` / `client.runs.events.list(run, opts)` | Page through run events. |
| `runs.events.subscribe(run, opts)` / `client.runs.events.subscribe(run, opts)` | Follow durable run events over SSE with cursor reconnects. |
| `tokens.create(opts)` / `client.tokens.create(opts)` | Create an externally completable token. Inside task code, `tokens.create()` returns a waitable runtime token handle. |
| `tokens.retrieve(id, opts)` / `client.tokens.retrieve(id, opts)` | Retrieve token metadata and completion result. |
| `tokens.list(opts)` / `client.tokens.list(opts)` | List tokens. |
| `tokens.complete(token, data, opts)` / `client.tokens.complete(token, data, opts)` | Complete a token with JSON data and return `{ status, token }`. Same canonical duplicate data returns `already_completed`; different data returns `token_completion_conflict`. |
| `tokens.cancel(token, opts)` / `client.tokens.cancel(token, opts)` | Cancel a pending token. |
| `schedules.list(opts)` / `client.schedules.list(opts)` | List schedules in a project environment. |
| `schedules.retrieve(idOrSchedule, opts)` / `client.schedules.retrieve(idOrSchedule, opts)` | Fetch one schedule. |

The top-level `sessions`, `runs`, `workspaces`, `tokens`, `schedules`, and `auth` facades mirror the client namespaces and use the default client from `HELMR_API_URL` and `HELMR_API_KEY`. Use `new HelmrClient(...)` when the caller needs explicit credentials or multiple control-plane targets. Imported task definitions are typed targets for the sessions namespace; they do not expose direct `.start()` or `.startAndWait()` helpers.

Session handles are explicit. `sessions.open("session-id")` treats the string as a session id only. Use `sessions.open({ externalId })` or `sessions.retrieve({ externalId })` when the caller knows the environment-scoped external conversation id; the SDK sends that external id as a server-resolved session address. Session starts use the same `externalId` as the retry-safe resource identity; they do not expose a separate start-request idempotency key.

Session start `payload` is persisted as audit data in the control plane. Put
secret values in declared `secrets`, not in payload. Stable interactive
workflows use Actor input/output; direct Task Run payload is not a follow-up
message channel.

Session starts create or reuse a session and attach a workspace. When no
workspace is supplied, Helmr creates one from the deployed task's sandbox.
Direct workspace operations are separate: creating an exec or PTY on a
workspace does not create a session or run.

Workspace file APIs read persisted version artifacts. Omitting `source` is the
same as `source: "current"` and reads the workspace's ready current version.
Use `{ source: "version", versionId }` to read a specific ready version in the
same workspace. Passing `versionId` without `source: "version"` is rejected.
`source: "live"` is reserved and returns not implemented until live file reads
are available. `files.list()` accepts `limit` up to 500 with a default of 200.
`versions.list()` accepts `limit` up to 200 with a default of 100.

Each Actor has exactly two durable channels, `input` and `output`. They are not
named or separately declared. Tagged JSON unions model application-level
message kinds. Actor-channel browser grants are deferred; use a Token for one
externally completed approval value.

`client.runs.wait()` follows the durable run-event stream and uses run snapshots as the convergence source of truth. If the event stream disconnects, it reconnects from the last event cursor. If a malformed SSE frame is detected while waiting, the client falls back to snapshots instead of failing the wait.

`client.runs.events.subscribe()` is the event-fidelity API. It reconnects from the last cursor and ends after a terminal run event. If a malformed SSE frame includes a valid event cursor, the client advances past that frame and reconnects; malformed frames without a cursor fail the iterator.

Run snapshots can include deployment and provenance metadata: `version`, `deploymentVersion`, `apiVersion`, `sdkVersion`, and `cliVersion`. Use `deploymentVersion` to reason about which deployed code snapshot ran. Use `apiVersion`, `sdkVersion`, and `cliVersion` for support/debugging rather than application logic.

Run snapshots also include `attemptNumber`. It is `null` before the run is leased by a worker, then starts at `1`. Use it to correlate run logs, events, and worker execution records for the same task execution attempt.

Workspace exec stdout/stderr and PTY output are durable cursor streams.
`list()` returns stored chunks after a cursor. `stream()` follows the same
stream over SSE and reconnects from the last received cursor.

Schedules use cron and generated schedule metadata payloads. They are defined
only with `schedules.task()` and reconciled by Deployment promotion. External
Schedule APIs are observational; source owns timing, Workspace address, and
lifecycle.
