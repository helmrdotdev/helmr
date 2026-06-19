---
title: Runtime client
description: TypeScript client APIs for starting tasks and inspecting sessions and runs.
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

Authenticated SDK calls require an API key. Scoped public access tokens can be used as raw HTTP bearer tokens for the single session channel or waitpoint action they grant. `http://` is allowed only for loopback hosts.

The SDK sends a pinned `Helmr-API-Version` header and `Helmr-SDK-Version` on every request. The pinned API version is the contract the SDK was built and tested against; it does not change with the current date.

Main surfaces:

| API | Purpose |
| --- | --- |
| `client.tasks.start<typeof task>(id, payload, opts)` | Start or reuse a task session by task id and return the session plus first/current run handle. |
| `client.tasks.startAndWait<typeof task>(id, payload, opts)` | Start or reuse a task session, then wait for the session to become terminal or time out. |
| `client.sessions.retrieve(session)` | Fetch current task session state. |
| `client.sessions.wait(session, opts)` | Wait for terminal task session state. |
| `client.sessions.open(session).input(channel).send(data)` | Append durable input to a session channel. |
| `client.sessions.open(session).output(channel).list(opts)` | Read durable session output records from a cursor. |
| `client.sessions.open(session).output(channel).stream(opts)` | Stream durable session output records over SSE. |
| `client.auth.createPublicToken(opts)` | Create a scoped opaque bearer token for one session input append or output read grant. |
| `client.runs.retrieve(run)` | Fetch current run snapshot. |
| `client.runs.wait(run, opts)` | Wait for terminal status using durable run events. |
| `client.runs.list(opts)` | List run summaries. |
| `client.runs.logs.retrieve(run)` | Read latest stdout/stderr snapshot. |
| `client.runs.events.list(run, opts)` | Page through run events. |
| `client.runs.events.subscribe(run, opts)` | Follow durable run events over SSE with cursor reconnects. |
| `client.runs.waitpoints.list(run, opts)` | Page through durable waitpoints. |
| `client.waitpoints.tokens.create(opts)` / `wait.createToken(opts)` | Create an externally completable waitpoint token. |
| `client.waitpoints.tokens.retrieve(id, opts)` / `wait.retrieveToken(id, opts)` | Retrieve waitpoint token metadata and completion result. |
| `client.waitpoints.tokens.list(opts)` / `wait.listTokens(opts)` | List waitpoint tokens. |
| `client.waitpoints.tokens.complete(token, data, opts)` / `wait.completeToken(token, data, opts)` | Complete a waitpoint token with JSON data. |
| `client.schedules.create(opts)` | Create an imperative cron schedule for a deployed task. |
| `client.schedules.list(opts)` | List schedules in a project environment. |
| `client.schedules.retrieve(id, opts)` | Fetch one schedule. |
| `client.schedules.update(id, opts)` | Update an imperative schedule. |
| `client.schedules.activate(id, opts)` | Activate an imperative schedule. |
| `client.schedules.deactivate(id, opts)` | Deactivate an imperative schedule. |
| `client.schedules.delete(id, opts)` | Delete an imperative schedule. |

Task start `payload` is persisted as audit data in the control plane. Put secret values in declared `secrets`, not in payload. Follow-up user messages, webhooks, or operator replies belong in session channel input, not in task start payload.

Create public access tokens with explicit resource bindings. A session input grant can append only to the bound session channel; a session output grant can read only from the bound session channel. Use separate tokens for read and write grants.

```ts
const outputToken = await client.auth.createPublicToken({
  scope: {
    type: "session.output.read",
    sessionId: session.id,
    channel: "agent.report",
    correlationId: "thread-1",
  },
  maxUses: 100,
})
```

`client.runs.wait()` follows the durable run-event stream and uses run snapshots as the convergence source of truth. If the event stream disconnects, it reconnects from the last event cursor. If a malformed SSE frame is detected while waiting, the client falls back to snapshots instead of failing the wait.

`client.runs.events.subscribe()` is the event-fidelity API. It reconnects from the last cursor and ends after a terminal run event. If a malformed SSE frame includes a valid event cursor, the client advances past that frame and reconnects; malformed frames without a cursor fail the iterator.

Run snapshots can include deployment and provenance metadata: `version`, `deploymentVersion`, `apiVersion`, `sdkVersion`, and `cliVersion`. Use `deploymentVersion` to reason about which deployed code snapshot ran. Use `apiVersion`, `sdkVersion`, and `cliVersion` for support/debugging rather than application logic.

Run snapshots also include `attemptNumber`. It is `null` before the run is leased by a worker, then starts at `1`. Automatic task retries are not part of the current pre-release API contract; the attempt number is reserved as the stable identity for future retry-aware logs and events.

Schedules use cron and generated schedule metadata payloads. `client.schedules.create()` accepts required `deduplicationKey`, `task`, and `cron`, plus optional `externalId`, `timezone`, `active`, and schedule run `options` such as `queue`, `concurrencyKey`, `priority`, `ttl`, and `maxDurationSeconds`. Scheduled starts resolve the current deployment for the task when they fire. `deduplicationKey` is the stable public key that prevents duplicate logical schedules: creating again with the same key updates the existing project-level schedule and selected environment instance. It does not accept arbitrary payload, secret bindings, workspace source, deployment pinning, or user-supplied idempotency controls. Declarative schedules are defined with `schedules.task()` and reconciled by deployment promotion, not by the imperative schedule methods.
