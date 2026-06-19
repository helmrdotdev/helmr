import type { ChannelRecord, PublicAccessToken, TaskSessionSnapshot, TaskStartResult, HelmrClient, WaitpointToken } from "./client"
import type { PendingWaitpoint, RunEventRecord, RunHandle, RunSnapshot } from "./run"
import type { Task } from "../internal"
import { idempotencyKeys, image, sandbox, schedules, source, task, wait } from "../index"

declare const client: HelmrClient
declare const handle: RunHandle
declare const snapshot: RunSnapshot
declare const pendingWaitpoint: PendingWaitpoint
declare const startTask: Task<{ issue: number }, { issue: number }, readonly []>
declare const schemaStartTask: Task<{ issue: number }, { parsed: number }, readonly [], { issue: string }>
declare const signal: AbortSignal

if (false) {
  const started: Promise<TaskStartResult<{ issue: number }>> = client.tasks.start<typeof startTask>(
    "inspect",
    { issue: 123 },
    {
      externalId: "case-123",
      idempotencyKey: "issue-123",
      idempotencyKeyTTL: "24h",
    },
  )
  const startAndWait: Promise<TaskSessionSnapshot<{ issue: number }>> = client.tasks.startAndWait<typeof startTask>(
    "inspect",
    { issue: 123 },
    { timeoutSeconds: 30 },
  )
  const startedAgain: Promise<TaskStartResult<{ issue: number }>> = client.tasks.start<typeof startTask>(
    "inspect",
    { issue: 123 },
    {
      idempotencyKey: "issue-123",
      idempotencyKeyTTL: "24h",
    },
  )
  const helperKey = idempotencyKeys.create(["issue", "123"], { scope: "global" })
  const schemaStartedRun: Promise<TaskStartResult<{ parsed: number }>> = schemaStartTask.start(
    { issue: "123" },
    { projectId: "project-1", environmentId: "env-1", idempotencyKey: helperKey },
  )
  const noPayloadTask = task({
    id: "no-payload",
    sandbox: sandbox("no-payload").image(image("no-payload").from("debian:trixie-slim")),
    run: async (ctx) => ({ runId: ctx.run.id }),
  })
  const rawLeaseEventRecord: RunEventRecord = {
    id: "event-1",
    run_id: "run-1",
    run_lease_id: "lease-1",
    kind: "run",
    message: "run.started",
    at: "2026-04-28T00:00:00Z",
    attributes: {},
  }
  const noPayloadStartedRun: Promise<TaskStartResult<{ runId: string }>> = noPayloadTask.start({})
  const session = client.sessions.open<{ ok: boolean }>("session-1")
  const sessionSnapshot: Promise<TaskSessionSnapshot<{ ok: boolean }>> = session.retrieve()
  const waitedSession: Promise<TaskSessionSnapshot<{ ok: boolean }>> = client.sessions.wait("session-1", { timeoutSeconds: 30 })
  const inputRecord: Promise<ChannelRecord<{ approved: boolean }>> = session.input("approval").send({ approved: true }, {
    correlationId: "thread-1",
  })
  const outputRecords: Promise<ChannelRecord<{ text: string }>[]> = session.output("agent.report").list({ cursor: 1 })
  const outputStream: Promise<AsyncIterable<ChannelRecord<{ text: string }>>> = session.output("agent.report").stream()
  const outputToken: Promise<PublicAccessToken> = client.auth.createPublicToken({
    scope: {
      type: "session.output.read",
      sessionId: "session-1",
      channel: "agent.report",
      correlationId: "thread-1",
    },
    maxUses: 10,
  })
  const inputToken: Promise<PublicAccessToken> = client.auth.createPublicToken({
    scope: {
      type: "session.input.append",
      sessionId: session.id,
      channel: "approval",
    },
  })
  const retrievedFromHandle: Promise<RunSnapshot> = client.runs.retrieve(handle)
  const retrievedFromId: Promise<RunSnapshot> = client.runs.retrieve("run-1")
  const waitedFromHandle: Promise<RunSnapshot> = client.runs.wait(handle, {
    timeoutMs: 30_000,
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
    deduplicationKey: "inspect-customer-1",
    task: "inspect",
    externalId: "customer-1",
    cron: "0 * * * *",
    active: false,
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
    run: async (payload, ctx) => `${payload.scheduleId}:${ctx.run.id}`,
  })
  client.runs.waitpoints.list(handle)
  client.runs.waitpoints.listPage(handle, { status: "pending" })
  const delegatedToken: Promise<WaitpointToken> = wait.createToken({
    timeoutInSeconds: 3600,
    metadata: { recipient: "reviewer@example.com" },
  })
  const clientToken: Promise<WaitpointToken> = client.waitpoints.tokens.create({ timeoutInSeconds: 3600 })
  const delegatedById = wait.createToken({ timeoutAt: "2026-04-20T00:00:00Z" })
  wait.completeToken({
    id: "token-1",
    callbackUrl: "https://api.example.test/api/waitpoints/tokens/token-1/callback/raw-token",
    publicAccessToken: "raw-token",
    timeoutAt: "2026-04-20T00:00:00Z",
  }, { approved: true })
  wait.completeToken("token-1", { approved: false })
  wait.completeToken("token-1", { approved: true }, { publicAccessToken: "raw-token" })
  snapshot.pendingWaitpoint?.kind

  // Keep the declared promises live without executing this block.
  startedAgain.then
  retrievedFromHandle.then
  retrievedFromId.then
  waitedFromHandle.then
  waitedFromId.then
  delegatedToken.then
  delegatedById.then
  clientToken.then
  schemaStartedRun.then
  started.then
  startAndWait.then
  noPayloadStartedRun.then
  sessionSnapshot.then
  waitedSession.then
  inputRecord.then
  outputRecords.then
  outputStream.then
  outputToken.then
  inputToken.then
  rawLeaseEventRecord.id

  // @ts-expect-error runs.retrieve accepts a run id string or RunHandle only.
  client.runs.retrieve({ taskId: "inspect" })
  // @ts-expect-error runs.retrieve does not accept arbitrary id-only objects.
  client.runs.retrieve({ id: "run-1" })
  // @ts-expect-error completion metadata belongs in the completion data payload.
  wait.completeToken("token-1", { approved: true }, { metadata: { actor: "alice@example.com" } })
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
  client.runs.wait("run-1", {
    // @ts-expect-error runs.wait is event-backed and does not accept polling intervals.
    intervalMs: 250,
  })
  // @ts-expect-error leased is an internal transient status, not a public run filter.
  client.runs.list({ status: "leased" })
  // @ts-expect-error events.list uses pageSize because it follows every page.
  client.runs.events.list(handle, { limit: 50 })
  const rawEventRecord: RunEventRecord = {
    id: "event-1",
    run_id: "run-1",
    run_lease_id: "lease-1",
    kind: "run",
    message: "run.started",
    at: "2026-04-28T00:00:00Z",
    attributes: {},
  }
  rawEventRecord.id
  // @ts-expect-error token create options do not accept response actions.
  wait.createToken({ actions: ["skip"] })
  wait.createToken({
    timeoutInSeconds: 3600,
    // @ts-expect-error token creation accepts timeoutInSeconds or timeoutAt, not both.
    timeoutAt: "2026-04-20T00:00:00Z",
  })
  // @ts-expect-error token complete options do not accept action-specific fields.
  wait.completeToken("token-1", { approved: true }, { reason: "ok" })
  client.tasks.start<typeof startTask>("inspect", { issue: 123 }, {
    // @ts-expect-error start options do not accept source inputs.
    source: source.file("README.md"),
  })
  client.tasks.start<typeof startTask>("inspect", { issue: 123 }, {
    externalId: "case-123",
    idempotencyKey: "request-123",
  })
  client.tasks.start<typeof startTask>(
    "inspect",
    // @ts-expect-error task payload and session input are distinct surfaces.
    { input: { approved: true } },
    {},
  )
  session.input("approval").send({ approved: true })
  // @ts-expect-error session input appends do not accept task start idempotency keys.
  session.input("approval").send({ approved: true }, { idempotencyKey: "start-key" })
  client.auth.createPublicToken({
    scope: {
      // @ts-expect-error public token scopes are closed.
      type: "sessions:*",
      sessionId: "session-1",
      channel: "approval",
    },
  })
  // @ts-expect-error sessions.open requires a session id string or session handle.
  client.sessions.open({ runId: "run-1" })
  // @ts-expect-error run replay is not exposed.
  client.runs.replay("run-1")
  // @ts-expect-error old task trigger client surface is not exposed.
  client.tasks.trigger("inspect", { issue: 123 }, {})
  schemaStartTask.start(
    // @ts-expect-error schema-backed start accepts schema input, not parsed run payload.
    { issue: 123 },
    {},
  )
  noPayloadTask.start(
    {},
    // @ts-expect-error no-payload tasks accept options as the first argument, not payload.
    { idempotencyKey: "payload-not-options" },
  )
  // @ts-expect-error source helpers are only for image file/directory inputs.
  source.tar("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
  // @ts-expect-error source helpers use file() or directory().
  source.path(".")
}
