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

Authenticated SDK calls require an API key. Scoped public access tokens can be used as raw HTTP bearer tokens for the single session stream or token action they grant. `http://` is allowed only for loopback hosts.

The SDK sends a pinned `Helmr-API-Version` header and `Helmr-SDK-Version` on every request. The pinned API version is the contract the SDK was built and tested against; it does not change with the current date.

Main surfaces:

| API | Purpose |
| --- | --- |
| `client.sessions.start(taskObject, payload, opts)` / `client.sessions.start<typeof task>(id, payload, opts)` | Start or reuse a task session and return the session plus first/current run handle. Pass a task object for local payload validation and type inference; pass a string id for external boundaries or dynamic task ids. |
| `client.sessions.startAndWait(taskObject, payload, opts)` / `client.sessions.startAndWait<typeof task>(id, payload, opts)` | Start or reuse a task session, then wait for the first run to become terminal or time out. The session remains open unless explicitly closed or cancelled. Pass a task object for local payload validation and type inference; pass a string id for external boundaries or dynamic task ids. |
| `client.sessions.retrieve(session)` | Fetch current task session state. |
| `client.sessions.open(session).input(stream).send(data)` | Append durable input to a session stream. |
| `client.sessions.open(session).output(stream).list(opts)` | Read durable session output records from a cursor. |
| `client.sessions.open(session).output(stream).read(opts)` | Read one durable session output record from a cursor. |
| `client.auth.createPublicToken(opts)` | Create a scoped opaque bearer token for one session input append or output read grant. |
| `workspaces.create(opts)` / `client.workspaces.create(opts)` | Create a durable workspace from a deployed sandbox. |
| `workspaces.open(id)` / `client.workspaces.open(id)` | Create a lazy handle for a workspace. |
| `workspaces.retrieve(idOrHandle, opts)` / `client.workspaces.retrieve(idOrHandle, opts)` | Fetch current workspace state. |
| `workspaces.list(opts)` / `client.workspaces.list(opts)` | List workspaces in the selected project environment. |
| `workspaces.update(idOrHandle, opts)` / `client.workspaces.update(idOrHandle, opts)` | Update workspace metadata or tags. |
| `workspaces.delete(idOrHandle, opts)` / `client.workspaces.delete(idOrHandle, opts)` | Delete a workspace. |
| `workspaces.materialize(idOrHandle, opts)` / `client.workspaces.materialize(idOrHandle, opts)` | Request worker materialization for a workspace. |
| `workspaces.connect(idOrHandle, opts)` / `client.workspaces.connect(idOrHandle, opts)` | Connect to an existing or newly materialized workspace. |
| `workspaces.stop(idOrHandle, opts)` / `client.workspaces.stop(idOrHandle, opts)` | Stop the active materialization for a workspace. |
| `workspaces.open(id).exec(command, opts)` / `client.workspaces.open(id).exec(command, opts)` | Start a write-capable command in the workspace. |
| `workspaces.open(id).execs.list(opts)` / `client.workspaces.open(id).execs.list(opts)` | List execs for a workspace. |
| `workspaces.open(id).pty.create(opts)` / `client.workspaces.open(id).pty.create(opts)` | Start an interactive PTY in the workspace. |
| `workspaces.open(id).pty.list(opts)` / `client.workspaces.open(id).pty.list(opts)` | List PTYs for a workspace. |
| `client.runs.retrieve(run)` | Fetch current run snapshot. |
| `client.runs.wait(run, opts)` | Wait for terminal status using durable run events. |
| `client.runs.list(opts)` | List run summaries. |
| `client.runs.logs.retrieve(run)` | Read latest stdout/stderr snapshot. |
| `client.runs.events.list(run, opts)` | Page through run events. |
| `client.runs.events.subscribe(run, opts)` | Follow durable run events over SSE with cursor reconnects. |
| `client.tokens.create(opts)` / `tokens.create(opts)` | Create an externally completable token. |
| `client.tokens.retrieve(id, opts)` | Retrieve token metadata and completion result. |
| `client.tokens.list(opts)` | List tokens. |
| `client.tokens.complete(token, data, opts)` | Complete a token with JSON data. |
| `client.tokens.cancel(token, opts)` | Cancel a pending token. |
| `client.schedules.create(opts)` | Create an imperative cron schedule for a deployed task. |
| `client.schedules.list(opts)` | List schedules in a project environment. |
| `client.schedules.retrieve(id, opts)` | Fetch one schedule. |
| `client.schedules.update(id, opts)` | Update an imperative schedule. |
| `client.schedules.activate(id, opts)` | Activate an imperative schedule. |
| `client.schedules.deactivate(id, opts)` | Deactivate an imperative schedule. |
| `client.schedules.delete(id, opts)` | Delete an imperative schedule. |

The top-level `sessions.start(...)`, `sessions.startAndWait(...)`, and `workspaces.*` facades mirror the client methods and use the default client from `HELMR_API_URL` and `HELMR_API_KEY`. Imported task definitions are typed targets for the sessions namespace; they do not expose direct `.start()` or `.startAndWait()` helpers.

Session start `payload` is persisted as audit data in the control plane. Put secret values in declared `secrets`, not in payload. Follow-up user messages, webhooks, or operator replies belong in session input streams, not in session start payload.

Session starts create or reuse a task session and attach a workspace. When no
workspace is supplied, Helmr creates one from the deployed task's sandbox.
Direct workspace operations are separate: creating an exec or PTY on a
workspace does not create a task session or run.

Session streams are named lanes on a task session. Input streams accept
follow-up records. Output streams expose task-published records through list
and read APIs.

Create public access tokens with explicit resource bindings. A session input grant can append only to the bound session stream; a session output grant can read only from the bound session stream. Use separate tokens for read and write grants.

```ts
const outputToken = await client.auth.createPublicToken({
  scope: {
    type: "session.output.read",
    sessionId: session.id,
    stream: "agent.report",
    correlationId: "thread-1",
  },
  maxUses: 100,
})
```

`client.runs.wait()` follows the durable run-event stream and uses run snapshots as the convergence source of truth. If the event stream disconnects, it reconnects from the last event cursor. If a malformed SSE frame is detected while waiting, the client falls back to snapshots instead of failing the wait.

`client.runs.events.subscribe()` is the event-fidelity API. It reconnects from the last cursor and ends after a terminal run event. If a malformed SSE frame includes a valid event cursor, the client advances past that frame and reconnects; malformed frames without a cursor fail the iterator.

Run snapshots can include deployment and provenance metadata: `version`, `deploymentVersion`, `apiVersion`, `sdkVersion`, and `cliVersion`. Use `deploymentVersion` to reason about which deployed code snapshot ran. Use `apiVersion`, `sdkVersion`, and `cliVersion` for support/debugging rather than application logic.

Run snapshots also include `attemptNumber`. It is `null` before the run is leased by a worker, then starts at `1`. Use it to correlate run logs, events, and worker execution records for the same task execution attempt.

Workspace exec stdout/stderr and PTY output are durable cursor streams.
`list()` returns stored chunks after a cursor. `stream()` follows the same
stream over SSE and reconnects from the last received cursor.

Schedules use cron and generated schedule metadata payloads. `client.schedules.create()` accepts required `deduplicationKey`, `task`, and `cron`, plus optional `externalId`, `timezone`, `active`, and schedule run `options` such as `queue`, `concurrencyKey`, `priority`, `ttl`, and `maxDurationSeconds`. Scheduled starts resolve the current deployment for the task when they fire. `deduplicationKey` is the stable public key that prevents duplicate logical schedules: creating again with the same key updates the existing project-level schedule and selected environment instance. Scheduled runs receive Helmr-generated schedule metadata rather than a caller-supplied payload. Declarative schedules are defined with `schedules.task()` and reconciled by deployment promotion, not by the imperative schedule methods.
