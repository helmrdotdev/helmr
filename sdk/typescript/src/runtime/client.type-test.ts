import type { PublicAccessToken, Schedule, SessionSnapshot, SessionStartResult, SessionStartAndWaitResult, HelmrClient, Token, TokenCompleteResult, Workspace, WorkspaceFileEntry, WorkspaceFileList, WorkspaceVersion } from "./client"
import type { RunEventRecord, RunHandle, RunSnapshot } from "./run"
import type { StreamRecord, Task } from "../internal"
import { auth, idempotencyKeys, image, queue, sandbox, schedules, sessions, source, streams, task, tokens, workspaces, type PayloadSchema } from "../index"

declare const client: HelmrClient
declare const handle: RunHandle
declare const taskWithPayload: Task<{ issue: number }, { issue: number }, readonly []>
declare const schemaTask: Task<{ issue: number }, { parsed: number }, readonly [], { issue: string }>
declare const signal: AbortSignal
declare const approvalSchema: PayloadSchema<{ approved: boolean }, { approved: boolean }>
declare const reportSchema: PayloadSchema<{ text: string }, { text: string }>

if (false) {
  const started: Promise<SessionStartResult<{ issue: number }>> = client.sessions.start<typeof taskWithPayload>(
    "inspect",
    { issue: 123 },
    {
      externalId: "case-123",
      idempotencyKey: "issue-123",
      idempotencyKeyTTL: "24h",
    },
  )
  const startAndWait: Promise<SessionStartAndWaitResult<{ issue: number }>> = client.sessions.startAndWait<typeof taskWithPayload>(
    "inspect",
    { issue: 123 },
    { timeoutSeconds: 30 },
  )
  const startedAgain: Promise<SessionStartResult<{ issue: number }>> = client.sessions.start<typeof taskWithPayload>(
    "inspect",
    { issue: 123 },
    {
      idempotencyKey: "issue-123",
      idempotencyKeyTTL: "24h",
    },
  )
  const helperKey = idempotencyKeys.create(["issue", "123"], { scope: "global" })
  const schemaStartedRun: Promise<SessionStartResult<{ parsed: number }>> = sessions.start(
    schemaTask,
    { issue: "123" },
    { projectId: "project-1", environmentId: "env-1", idempotencyKey: helperKey },
  )
  const clientSchemaStartedRun: Promise<SessionStartResult<{ parsed: number }>> = client.sessions.start(
    schemaTask,
    { issue: "123" },
    { projectId: "project-1", environmentId: "env-1", idempotencyKey: helperKey },
  )
  const schemaStartAndWait: Promise<SessionStartAndWaitResult<{ parsed: number }>> = sessions.startAndWait(
    schemaTask,
    { issue: "123" },
    { projectId: "project-1", environmentId: "env-1", timeoutSeconds: 30 },
  )
  const clientSchemaStartAndWait: Promise<SessionStartAndWaitResult<{ parsed: number }>> = client.sessions.startAndWait(
    schemaTask,
    { issue: "123" },
    { projectId: "project-1", environmentId: "env-1", timeoutSeconds: 30 },
  )
  const canonicalStartedRun: Promise<SessionStartResult<{ parsed: number }>> = client.sessions.start<typeof schemaTask>(
    schemaTask.id,
    { issue: "123" },
    { projectId: "project-1", environmentId: "env-1", idempotencyKey: helperKey },
  )
  const noPayloadTask = task({
    id: "no-payload",
    sandbox: sandbox("no-payload").image(image("no-payload").from("debian:trixie-slim")),
    run: async (ctx) => ({ runId: ctx.run.id }),
  })
  const serialQueue = queue({ id: "review/pr", concurrencyLimit: 1 })
  task({
    id: "queued-task",
    sandbox: sandbox("queued-task").image(image("queued-task").from("debian:trixie-slim")),
    queue: serialQueue,
    run: async () => null,
  })
  task({
    id: "dynamic-queue-ref",
    sandbox: sandbox("dynamic-queue-ref").image(image("dynamic-queue-ref").from("debian:trixie-slim")),
    queue: "review/pr",
    run: async () => null,
  })
  task({
    id: "bad-inline-queue",
    sandbox: sandbox("bad-inline-queue").image(image("bad-inline-queue").from("debian:trixie-slim")),
    // @ts-expect-error queue policy is defined with queue(...), not inline task config.
    queue: { concurrencyLimit: 1 },
    run: async () => null,
  })
  task({
    id: "bad-retry-disabled",
    sandbox: sandbox("bad-retry-disabled").image(image("bad-retry-disabled").from("debian:trixie-slim")),
    // @ts-expect-error retry policy is always an object.
    retry: false,
    run: async () => null,
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
  const noPayloadStartedRun: Promise<SessionStartResult<{ runId: string }>> = sessions.start(noPayloadTask, {})
  const clientNoPayloadStartedRun: Promise<SessionStartResult<{ runId: string }>> = client.sessions.start(noPayloadTask, {})
  const noPayloadStartAndWait: Promise<SessionStartAndWaitResult<{ runId: string }>> = sessions.startAndWait(noPayloadTask, {})
  const clientNoPayloadStartAndWait: Promise<SessionStartAndWaitResult<{ runId: string }>> = client.sessions.startAndWait(noPayloadTask, {})
  const session = client.sessions.open<{ ok: boolean }>("session-1")
  const externalSession = client.sessions.open({ externalId: "slack:T123:C456" })
  const topLevelSession = sessions.open<{ ok: boolean }>("session-1")
  const externalSessionExternalId: string = externalSession.externalId
  void externalSessionExternalId
  // @ts-expect-error external-id handles do not expose a session id before resolution.
  externalSession.id
  const sessionSnapshot: Promise<SessionSnapshot<{ ok: boolean }>> = session.retrieve()
  const topLevelSessionSnapshot: Promise<SessionSnapshot<{ ok: boolean }>> = sessions.retrieve("session-1")
  const externalSessionSnapshot: Promise<SessionSnapshot> = client.sessions.retrieve({ externalId: "slack:T123:C456" })
  const externalSessionList: Promise<SessionSnapshot[]> = client.sessions.list({ externalId: "slack:T123:C456" })
  void externalSessionList
  const approvalStream = streams.input("approval", { schema: approvalSchema })
  const reportStream = streams.output("agent.report", { schema: reportSchema })
  const inputRecord: Promise<StreamRecord<{ approved: boolean }>> = session.input(approvalStream).send({ approved: true }, {
    correlationId: "thread-1",
  })
  const outputRecords: Promise<StreamRecord<{ text: string }>[]> = session.output(reportStream).list({ cursor: 1 })
  const outputRecord: Promise<StreamRecord<{ text: string }> | null> = session.output(reportStream).read({ cursor: 1 })
  const outputToken: Promise<PublicAccessToken> = client.auth.createPublicToken({
    projectId: "project-1",
    environmentId: "env-1",
    scope: {
      type: "session.output.read",
      session: { externalId: "slack:T123:C456" },
      stream: "agent.report",
      correlationId: "thread-1",
    },
    maxUses: 10,
  })
  const inputToken: Promise<PublicAccessToken> = client.auth.createPublicToken({
    scope: {
      type: "session.input.send",
      session,
      stream: "approval",
    },
  })
  const topLevelOutputToken: Promise<PublicAccessToken> = auth.createPublicToken({
    scope: {
      type: "session.output.read",
      session: topLevelSession,
      stream: reportStream,
    },
  })
  const topLevelInputToken: Promise<PublicAccessToken> = auth.createPublicToken({
    scope: {
      type: "session.input.send",
      session: topLevelSession.id,
      stream: approvalStream,
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
  const workspace: Promise<Workspace> = workspaces.retrieve("workspace-1")
  const openedWorkspace = workspaces.open("workspace-1")
  const workspaceExec = openedWorkspace.exec(["bash", "-lc", "echo ok"])
  const fileReadCurrent: Promise<Uint8Array> = openedWorkspace.files.read("src/app.ts")
  const fileReadVersion: Promise<Uint8Array> = openedWorkspace.files.read("src/app.ts", { source: "version", versionId: "version-1" })
  const fileList: Promise<WorkspaceFileList> = openedWorkspace.files.list("src", { source: "version", versionId: "version-1", limit: 10 })
  const fileStatLive: Promise<WorkspaceFileEntry> = openedWorkspace.files.stat("src/app.ts", { source: "live", materializationId: "materialization-1" })
  const workspaceVersion: Promise<WorkspaceVersion> = openedWorkspace.versions.retrieve("version-1")
  const workspaceVersions: Promise<WorkspaceVersion[]> = openedWorkspace.versions.list({ kind: "user", limit: 10 })
  // @ts-expect-error versionId requires source: "version".
  openedWorkspace.files.read("src/app.ts", { versionId: "version-1" })
  // @ts-expect-error source: "version" requires versionId.
  openedWorkspace.files.read("src/app.ts", { source: "version" })
  // @ts-expect-error current source does not accept versionId.
  openedWorkspace.files.stat("src/app.ts", { source: "current", versionId: "version-1" })
  // @ts-expect-error version source does not accept materializationId.
  openedWorkspace.files.list("src", { source: "version", versionId: "version-1", materializationId: "materialization-1" })
  // @ts-expect-error live source does not accept versionId.
  openedWorkspace.files.stat("src/app.ts", { source: "live", versionId: "version-1" })
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
  const scheduleRecord = null as unknown as Schedule
  const scheduleByTopLevel: Promise<Schedule> = schedules.retrieve(scheduleRecord)
  const scheduleByClient: Promise<Schedule> = client.schedules.activate(scheduleRecord)
  schedules.update(scheduleRecord, {
    task: "inspect",
    externalId: "customer-1",
    cron: "30 * * * *",
  })
  schedules.task({
    id: "scheduled-task",
    sandbox: sandbox("scheduled-task").image(image("scheduled-task").from("debian:trixie-slim")),
    cron: { pattern: "0 9 * * *", timezone: "Asia/Tokyo" },
    run: async (payload, ctx) => `${payload.scheduleId}:${ctx.run.id}`,
  })
  const delegatedToken: Promise<Token> = tokens.create({
    timeout: "1h",
    metadata: { recipient: "reviewer@example.com" },
  })
  const clientToken: Promise<Token> = client.tokens.create({ timeout: "1h" })
  const retrievedTopLevelToken: Promise<Token> = tokens.retrieve("token-1")
  const listedTopLevelTokens: Promise<Token[]> = tokens.list()
  const retrievedClientToken = null as unknown as Token
  // @ts-expect-error client tokens are data records; task-runtime token handles expose wait().
  retrievedClientToken.wait()
  const delegatedById = tokens.create({ timeout: { hours: 1 } })
  const completedToken: Promise<TokenCompleteResult<{ approved: boolean }>> = tokens.complete("token-1", { approved: true })
  const cancelledTopLevelToken: Promise<Token> = tokens.cancel("token-1")
  const publicCompletedToken: Promise<TokenCompleteResult<{ approved: boolean }>> = client.tokens.complete({
    id: "token-1",
    callbackUrl: "https://api.example.test/api/v1/tokens/token-1/callback/raw-token",
    publicAccessToken: "raw-token",
    timeoutAt: "2026-04-20T00:00:00Z",
  }, { approved: true })
  client.tokens.complete("token-1", { approved: false })
  client.tokens.complete("token-1", { approved: true }, { publicAccessToken: "raw-token" })

  // Keep the declared promises live without executing this block.
  startedAgain.then
  topLevelSessionSnapshot.then
  topLevelOutputToken.then
  topLevelInputToken.then
  retrievedFromHandle.then
  retrievedFromId.then
  waitedFromHandle.then
  waitedFromId.then
  fileReadCurrent.then
  fileReadVersion.then
  fileList.then
  fileStatLive.then
  workspaceVersion.then
  workspaceVersions.then
  delegatedToken.then
  delegatedById.then
  completedToken.then
  publicCompletedToken.then
  retrievedTopLevelToken.then
  listedTopLevelTokens.then
  cancelledTopLevelToken.then
  scheduleByTopLevel.then
  scheduleByClient.then
  clientToken.then
  workspace.then
  workspaceExec.then
  schemaStartedRun.then
  clientSchemaStartedRun.then
  schemaStartAndWait.then
  clientSchemaStartAndWait.then
  canonicalStartedRun.then
  started.then
  startAndWait.then
  noPayloadStartedRun.then
  clientNoPayloadStartedRun.then
  noPayloadStartAndWait.then
  clientNoPayloadStartAndWait.then
  sessionSnapshot.then
  externalSession.retrieve().then
  externalSessionSnapshot.then
  inputRecord.then
  outputRecords.then
  outputRecord.then
  outputToken.then
  inputToken.then
  rawLeaseEventRecord.id

  // @ts-expect-error runs.retrieve accepts a run id string or RunHandle only.
  client.runs.retrieve({ taskId: "inspect" })
  // @ts-expect-error runs.retrieve does not accept arbitrary id-only objects.
  client.runs.retrieve({ id: "run-1" })
  // @ts-expect-error completion metadata belongs in the completion data payload.
  client.tokens.complete("token-1", { approved: true }, { metadata: { actor: "alice@example.com" } })
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
  tokens.create({ actions: ["skip"] })
  // @ts-expect-error token creation accepts DurationInput timeout, not timeoutInSeconds.
  tokens.create({ timeoutInSeconds: 3600 })
  // @ts-expect-error token complete options do not accept action-specific fields.
  client.tokens.complete("token-1", { approved: true }, { reason: "ok" })
  client.sessions.start<typeof taskWithPayload>("inspect", { issue: 123 }, {
    // @ts-expect-error start options do not accept source inputs.
    source: source.file("README.md"),
  })
  client.sessions.start<typeof taskWithPayload>("inspect", { issue: 123 }, {
    externalId: "case-123",
    idempotencyKey: "request-123",
  })
  client.sessions.start<typeof taskWithPayload>(
    "inspect",
    // @ts-expect-error task payload and session input are distinct surfaces.
    { input: { approved: true } },
    {},
  )
  session.input("approval").send({ approved: true })
  session.input("approval").send({ approved: true }, { idempotencyKey: "start-key" })
  client.auth.createPublicToken({
    scope: {
      // @ts-expect-error public token scopes are closed.
      type: "sessions:*",
      session: "session-1",
      stream: "approval",
    },
  })
  client.auth.createPublicToken({
    scope: {
      type: "session.input.send",
      // @ts-expect-error public token session scope uses `session`, not legacy `sessionId`.
      sessionId: "session-1",
      stream: "approval",
    },
  })
  auth.createPublicToken({
    // @ts-expect-error input grants require an input stream or string.
    scope: {
      type: "session.input.send",
      session: "session-1",
      stream: reportStream,
    },
  })
  auth.createPublicToken({
    // @ts-expect-error output grants require an output stream or string.
    scope: {
      type: "session.output.read",
      session: "session-1",
      stream: approvalStream,
    },
  })
  // @ts-expect-error sessions.open requires a session id string, session handle, or explicit externalId address.
  client.sessions.open({ runId: "run-1" })
  // @ts-expect-error run replay is not exposed.
  client.runs.replay("run-1")
  // @ts-expect-error old task trigger client surface is not exposed.
  client.sessions.trigger("inspect", { issue: 123 }, {})
  sessions.start(
    schemaTask,
    // @ts-expect-error schema-backed start accepts schema input, not parsed run payload.
    { issue: 123 },
    {},
  )
  sessions.startAndWait(
    schemaTask,
    // @ts-expect-error schema-backed startAndWait accepts schema input, not parsed run payload.
    { issue: 123 },
    {},
  )
  sessions.start(
    noPayloadTask,
    {},
    // @ts-expect-error no-payload tasks accept options as the first argument, not payload.
    { idempotencyKey: "payload-not-options" },
  )
  sessions.startAndWait(
    noPayloadTask,
    {},
    // @ts-expect-error no-payload tasks accept options as the first argument, not payload.
    { timeoutSeconds: 30 },
  )
  // @ts-expect-error task definitions do not expose direct start helpers.
  schemaTask.start
  // @ts-expect-error task definitions do not expose direct startAndWait helpers.
  schemaTask.startAndWait
  // @ts-expect-error source helpers are only for image file/directory inputs.
  source.tar("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
  // @ts-expect-error source helpers use file() or directory().
  source.path(".")
}
