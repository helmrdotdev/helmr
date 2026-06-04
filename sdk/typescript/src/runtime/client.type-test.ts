import type { HelmrClient, WaitpointResponseToken } from "./client"
import type { PendingDelayWaitpoint, PendingManualWaitpoint, RunHandle, RunSnapshot } from "./run"
import type { Task } from "../internal"
import { idempotencyKeys, image, sandbox, schedules, source, task } from "../index"

declare const client: HelmrClient
declare const handle: RunHandle
declare const snapshot: RunSnapshot
declare const pendingManual: PendingManualWaitpoint
declare const pendingDelay: PendingDelayWaitpoint
declare const triggerTask: Task<{ issue: number }, { issue: number }, {}>
declare const schemaTriggerTask: Task<{ issue: number }, { parsed: number }, Record<never, never>, { issue: string }>
declare const signal: AbortSignal

if (false) {
  const triggered: Promise<RunHandle> = client.tasks.trigger<typeof triggerTask>(
    "inspect",
    { issue: 123 },
    {
      idempotencyKey: "issue-123",
      idempotencyKeyTTL: "24h",
    },
  )
  const helperKey = idempotencyKeys.create(["issue", "123"], { scope: "global" })
  const schemaTriggered: Promise<RunHandle<{ parsed: number }>> = schemaTriggerTask.trigger(
    { issue: "123" },
    { idempotencyKey: helperKey },
  )
  const noPayloadTask = task({
    id: "no-payload",
    sandbox: sandbox("no-payload").image(image("no-payload").from("debian:trixie-slim")),
    run: async (ctx) => ({ runId: ctx.run.id }),
  })
  const noPayloadTriggered: Promise<RunHandle<{ runId: string }>> = noPayloadTask.trigger({})
  const retrievedFromHandle: Promise<RunSnapshot> = client.runs.retrieve(handle)
  const retrievedFromId: Promise<RunSnapshot> = client.runs.retrieve("run-1")
  const waitedFromHandle: Promise<RunSnapshot> = client.runs.wait(handle, {
    timeoutMs: 30_000,
    intervalMs: 250,
    signal,
  })
  const waitedFromId: Promise<RunSnapshot> = client.runs.wait("run-1")
  client.runs.logs.retrieve(handle)
  client.runs.logs.retrieve("run-1")
  client.runs.events.list(handle)
  client.runs.events.list(handle, { pageSize: 50 })
  client.runs.events.list("run-1")
  client.runs.events.subscribe(handle)
  client.runs.events.subscribe("run-1")
  client.runs.list({ status: "running" })
  client.schedules.create({
    task: "inspect",
    externalId: "customer-1",
    cron: "0 * * * *",
    active: false,
    secretBindings: {
      API_TOKEN: "vault:api-token",
    },
    options: {
      maxDurationSeconds: 600,
    },
  })
  client.schedules.update("schedule-1", {
    task: "inspect",
    externalId: "customer-1",
    cron: "15 * * * *",
  })
  schedules.task({
    id: "scheduled-task",
    sandbox: sandbox("scheduled-task").image(image("scheduled-task").from("debian:trixie-slim")),
    cron: { pattern: "0 9 * * *", timezone: "Asia/Tokyo" },
    secretBindings: {
      API_TOKEN: "vault:api-token",
    },
    run: async (payload, ctx) => `${payload.scheduleId}:${ctx.run.id}`,
  })
  client.waitpoints.respond(pendingManual, { value: { approved: true } })
  client.waitpoints.respond("waitpoint-1", { value: { approved: true } })
  const delegatedToken: Promise<WaitpointResponseToken> = client.waitpoints.tokens.create(pendingManual, {
    expiresInSeconds: 3600,
    metadata: { recipient: "reviewer@example.com" },
  })
  const delegatedById = client.waitpoints.tokens.create(
    { waitpointId: "waitpoint-1" },
    { expiresAt: "2026-04-20T00:00:00Z" },
  )
  client.waitpoints.tokens.create("waitpoint-1")
  client.waitpoints.tokens.respond({
    id: "token-1",
    waitpointId: "waitpoint-1",
    url: "https://api.example.test/waitpoints/respond?id=token-1&token=raw-token",
    token: "raw-token",
    expiresAt: null,
  }, {
    value: { approved: true },
    externalSubject: "alice@example.com",
    metadata: { source: "email" },
  })
  client.waitpoints.tokens.respond("token-1", "raw-token", { value: { approved: false } })
  snapshot.pendingWaitpoint?.kind

  // Keep the declared promises live without executing this block.
  triggered.then
  retrievedFromHandle.then
  retrievedFromId.then
  waitedFromHandle.then
  waitedFromId.then
  delegatedToken.then
  delegatedById.then
  schemaTriggered.then
  noPayloadTriggered.then

  // @ts-expect-error runs.retrieve accepts a run id string or RunHandle only.
  client.runs.retrieve({ taskId: "inspect" })
  // @ts-expect-error runs.retrieve does not accept arbitrary id-only objects.
  client.runs.retrieve({ id: "run-1" })
  // @ts-expect-error runs.wait accepts a run id string or RunHandle only.
  client.runs.wait({ taskId: "inspect" })
  // @ts-expect-error runs.wait does not accept arbitrary id-only objects.
  client.runs.wait({ id: "run-1" })
  // @ts-expect-error logs.retrieve accepts a run id string or RunHandle only.
  client.runs.logs.retrieve({ id: "run-1" })
  // @ts-expect-error events.list accepts a run id string or RunHandle only.
  client.runs.events.list({ id: "run-1" })
  // @ts-expect-error events.subscribe accepts a run id string or RunHandle only.
  client.runs.events.subscribe({ id: "run-1" })
  // @ts-expect-error leased is an internal transient status, not a public run filter.
  client.runs.list({ status: "leased" })
  // @ts-expect-error events.list uses pageSize because it follows every page.
  client.runs.events.list(handle, { limit: 50 })
  // @ts-expect-error delay waitpoints cannot be responded to by a caller.
  client.waitpoints.respond(pendingDelay, { value: "done" })
  // @ts-expect-error token creation is only for caller-completable waitpoints.
  client.waitpoints.tokens.create(pendingDelay)
  // @ts-expect-error respond options do not accept action-specific fields.
  client.waitpoints.respond("waitpoint-1", { reason: "ok" })
  // @ts-expect-error token create options do not accept response actions.
  client.waitpoints.tokens.create(pendingManual, { actions: ["skip"] })
  // @ts-expect-error token creation accepts expiresInSeconds or expiresAt, not both.
  client.waitpoints.tokens.create(pendingManual, {
    expiresInSeconds: 3600,
    expiresAt: "2026-04-20T00:00:00Z",
  })
  // @ts-expect-error token response by id requires a token secret and options object.
  client.waitpoints.tokens.respond("token-1", "raw-token")
  // @ts-expect-error token response options do not accept action-specific fields.
  client.waitpoints.tokens.respond("token-1", "raw-token", { reason: "ok" })
  client.tasks.trigger<typeof triggerTask>(
    "inspect",
    { issue: 123 },
    {
    // @ts-expect-error trigger options do not accept source inputs.
    source: source.file("README.md"),
    },
  )
  schemaTriggerTask.trigger(
    // @ts-expect-error schema-backed triggers accept schema input, not parsed run payload.
    { issue: 123 },
    {},
  )
  noPayloadTask.trigger(
    {},
    // @ts-expect-error no-payload tasks accept options as the first argument, not payload.
    { idempotencyKey: "payload-not-options" },
  )
  // @ts-expect-error source helpers are only for image file/directory inputs.
  source.tar("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
  // @ts-expect-error source helpers use file() or directory().
  source.path(".")
}
