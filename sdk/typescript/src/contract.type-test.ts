import {
  actor,
  image,
  queue,
  source,
  task,
  sandbox,
  schedules,
  sessions,
  tokens,
  workspaces,
  type JsonValue,
  type HelmrClient,
  type Actor,
  type ActorStartResult,
  type ImageBuilder,
  type PayloadSchema,
  type ActorInfo,
  type ActorContext,
  type ActorSessionReceive,
  type SandboxInfo,
  type TaskContext,
  type TaskConfig,
  type TaskConfigWithPayload,
  type TaskConfigWithoutPayload,
  type TaskInfo,
  type TaskInput,
  type TaskOutput,
  type SessionRef,
  type TokenCreateResult,
  type TokenCancelRequest,
  type TokenCompleteRequest,
  type TokenRef,
  type Queue,
  type QueueConfig,
  type SandboxBuilder,
  type SandboxConfig,
  type SandboxResourceBuilder,
  type Sandbox,
  type SecretCreateRequest,
  type SecretRevokeRequest,
  type SecretRotateRequest,
  type SessionOutputWriter,
  type SourceDirectory,
  type SourceFile,
  type WorkspaceFiles,
  type WorkspaceRef,
  type WorkspaceSecretPlacement,
} from "."

const schema: PayloadSchema<{ readonly value: string }> = {
  "~standard": {
    version: 1,
    vendor: "type-test",
    validate(value) {
      return { value: value as { readonly value: string } }
    },
  },
}

export function assertGreenfieldTypes(): void {
  const queueConfig: QueueConfig = {
    name: "default",
    concurrencyLimit: 10,
  }
  const defaultQueue: Queue = queue(queueConfig)
  // @ts-expect-error Queue values must be created by queue().
  const unbrandedQueue: Queue = { name: "default", concurrencyLimit: 10 }
  void unbrandedQueue

  const runDefaults = {
    queue: defaultQueue,
    maxDuration: "30m",
    ttl: "1h",
    retry: { maxAttempts: 3 },
  } as const

  const sandboxConfig: SandboxConfig = { id: "machine" }
  const stagedSandbox: SandboxBuilder = sandbox(sandboxConfig)
  const stagedResources: SandboxResourceBuilder = stagedSandbox.image(
    image("staged-root").from("debian"),
  )
  stagedResources.resources({ cpu: 1, memory: "1GiB" })

  // @ts-expect-error ImageBuilder values must be created by image().
  const unbrandedImage: ImageBuilder = {
    key: "unbranded",
    from: () => null as never,
    run: () => null as never,
    copy: () => null as never,
    copyFrom: () => null as never,
    workdir: () => null as never,
    env: () => null as never,
    user: () => null as never,
  }
  void unbrandedImage

  const writeValues = async (
    writer: SessionOutputWriter,
    values: readonly JsonValue[],
  ): Promise<void> => {
    for (const value of values) await writer.write(value)
    await writer.close()
  }
  const listRoot = (files: WorkspaceFiles) => files.list(".")
  void writeValues
  void listRoot

  const placement: WorkspaceSecretPlacement = { env: "TOKEN" }
  const secretCreate: SecretCreateRequest = {
    name: "TOKEN",
    value: "secret",
  }
  const secretRotate: SecretRotateRequest = {
    value: "replacement",
    idempotencyKey: "rotate-token",
  }
  const secretRevoke: SecretRevokeRequest = {
    idempotencyKey: "revoke-token",
  }
  const tokenComplete: TokenCompleteRequest = { result: null }
  const tokenCancel: TokenCancelRequest = {}
  void placement
  void secretCreate
  void secretRotate
  void secretRevoke
  void tokenComplete
  void tokenCancel

  const builder = sandbox({ id: "machine" })
  // @ts-expect-error image is required before resources.
  builder.resources({ cpu: 1, memory: "1GiB" })

  const resourceBuilder = builder.image(image("root").from("debian"))
  // @ts-expect-error registry authentication belongs to the local BuildKit session.
  image("private").from("ghcr.io/acme/base:1", { auth: {} })
  const sourceFile: SourceFile = source.file("./package.json")
  const sourceDirectory: SourceDirectory = source.directory("./src")
  image("source-copy")
    .copy("/app/package.json", sourceFile)
    .copy("/app/src", sourceDirectory)
  // @ts-expect-error Source files must be created by source.file().
  const unbrandedSourceFile: SourceFile = { path: "./package.json" }
  // @ts-expect-error Source directories must be created by source.directory().
  const unbrandedSourceDirectory: SourceDirectory = { path: "./src" }
  void unbrandedSourceFile
  void unbrandedSourceDirectory
  // @ts-expect-error v0 directory copy has no implementation-defined ignore language.
  source.directory("./src", { ignore: ["**/*.test.ts"] })
  // @ts-expect-error memory is required.
  resourceBuilder.resources({ cpu: 1 })
  // @ts-expect-error memory uses canonical MiB or GiB suffixes.
  resourceBuilder.resources({ cpu: 1, memory: "1Gi" })
  // @ts-expect-error v0 ephemeral disk capacity is not public input.
  resourceBuilder.resources({ cpu: 1, memory: "1GiB", disk: "64GiB" })

  const machine: Sandbox = resourceBuilder.resources({
    cpu: 1,
    memory: "1GiB",
  })
  // @ts-expect-error Sandbox values must be created by sandbox().
  const unbrandedSandbox: Sandbox = {
    id: "unbranded",
    createWorkspace: async () => null as never,
  }
  void unbrandedSandbox

  const payloadTask = task({
    id: "payload",
    ...runDefaults,
    payload: schema,
    run(payload, ctx): JsonValue {
      ctx satisfies TaskContext
      return payload
    },
  })
  payloadTask.id satisfies "payload"
  const payloadInput: TaskInput<typeof payloadTask> = { value: "ok" }
  payloadInput.value satisfies string
  const runtimeWorkspace = workspaces.ref(
    "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
  )
  runtimeWorkspace satisfies WorkspaceRef
  payloadTask.start(
    { value: "ok" },
    { workspace: runtimeWorkspace },
  )
  const childWait = payloadTask.call(
    { value: "ok" },
    {
      workspace: runtimeWorkspace,
      idempotencyKey: "payload:ok",
    },
  )
  childWait.unwrap().then((output) => {
    output satisfies JsonValue
  })
  payloadTask.call(
    { value: "ok" },
    // @ts-expect-error task.call requires an explicit idempotency key.
    { workspace: runtimeWorkspace },
  )
  // @ts-expect-error a payload-bearing task call requires its payload position.
  payloadTask.call({
    workspace: runtimeWorkspace,
    idempotencyKey: "payload:missing",
  })
  // @ts-expect-error a payload-bearing task always requires payload.
  payloadTask.start({ workspace: runtimeWorkspace })
  const client = null as unknown as HelmrClient
  client.tasks.retrieve("payload").then((info: TaskInfo) => {
    info.id satisfies string
    info.deploymentId satisfies string
  })
  client.actors.retrieve("operator").then((info: ActorInfo) => {
    info.id satisfies string
    info.deploymentId satisfies string
  })
  client.sandboxes.retrieve("machine").then((info: SandboxInfo) => {
    info.id satisfies string
    info.deploymentId satisfies string
  })
  const clientWorkspace = client.workspaces.ref(
    "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
  )
  clientWorkspace satisfies WorkspaceRef
  client.sandboxes.createWorkspace("machine").then((workspace) => {
    workspace satisfies WorkspaceRef
  })
  sessions.ref(
    "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
  ) satisfies SessionRef
  client.sessions.ref(
    "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
  ) satisfies SessionRef
  tokens.create().then((token) => {
    token satisfies TokenCreateResult & TokenRef
  })
  client.tokens.create().then((token) => {
    token satisfies TokenCreateResult
    // @ts-expect-error external Token creation returns credentials, not a runtime Wait handle.
    token.wait()
  })
  client.tasks.start<typeof payloadTask>("payload", {
    payload: { value: "ok" },
    workspace: clientWorkspace,
  })
  client.tasks.start<typeof payloadTask>(
    // @ts-expect-error a typed Task start requires that Task's declared ID.
    "other-task",
    {
      payload: { value: "ok" },
      workspace: clientWorkspace,
    },
  )
  // @ts-expect-error the external typed Task envelope requires payload.
  client.tasks.start<typeof payloadTask>("payload", {
    workspace: clientWorkspace,
  })
  const typedOutputTask = task({
    id: "typed-output",
    payload: schema,
    run(payload) {
      return { value: payload.value, count: payload.value.length }
    },
  })
  const typedRun = client.tasks.start<typeof typedOutputTask>("typed-output", {
    payload: { value: "ok" },
    workspace: clientWorkspace,
  })
  type TypedOutput = TaskOutput<typeof typedOutputTask>
  const typedOutput: TypedOutput = { value: "ok", count: 2 }
  typedOutput.count satisfies number
  typedRun.then((handle) => {
    client.runs.retrieve(handle).then((run) => {
      if (run.output !== undefined) {
        run.output.value satisfies string
        run.output.count satisfies number
      }
    })
    const payloadTaskWait = client.runs.wait(handle)
    payloadTaskWait.unwrap().then((output) => {
      output.value satisfies string
      output.count satisfies number
      // @ts-expect-error the Task output has no unknown member.
      output.missing
    })
    payloadTaskWait.then((result) => {
      if (result.ok) {
        result.output.value satisfies string
      } else {
        result.failure.code satisfies string
      }
    })
  })

  const noPayloadTask = task({
    id: "no-payload",
    run(): JsonValue {
      return null
    },
  })
  const payloadConfig = {
    id: "payload-config",
    payload: schema,
    run(payload: { readonly value: string }) {
      return payload
    },
  } satisfies TaskConfigWithPayload<
    "payload-config",
    { readonly value: string },
    { readonly value: string },
    { readonly value: string }
  >
  task(payloadConfig)
  const noPayloadConfig = {
    id: "no-payload-config",
    run: () => null,
  } satisfies TaskConfigWithoutPayload<"no-payload-config", null>
  task(noPayloadConfig)
  noPayloadTask.id satisfies "no-payload"
  const configuredTask = {
    id: "configured",
    run: () => null,
  } satisfies TaskConfig<"configured", never, never, null>
  task(configuredTask).id satisfies "configured"
  noPayloadTask.start({ workspace: runtimeWorkspace })
  noPayloadTask.call({
    workspace: runtimeWorkspace,
    idempotencyKey: "no-payload",
  })
  // @ts-expect-error task.call requires an explicit idempotency key.
  noPayloadTask.call({ workspace: runtimeWorkspace })
  client.tasks.start<typeof noPayloadTask>("no-payload", {
    workspace: clientWorkspace,
  })
  client.tasks.start<typeof noPayloadTask>("no-payload", {
    // @ts-expect-error the external no-payload Task envelope forbids payload.
    payload: null,
    workspace: clientWorkspace,
  })
  // @ts-expect-error a no-payload task has no payload position.
  noPayloadTask.start(null, { workspace: runtimeWorkspace })
  // @ts-expect-error a no-payload task call has no payload position.
  noPayloadTask.call(null, {
    workspace: runtimeWorkspace,
    idempotencyKey: "no-payload:unexpected",
  })

  const scheduled = schedules.task({
    id: "scheduled",
    cron: { pattern: "0 * * * *", timezone: "UTC" },
    workspace: { sandbox: machine },
    run(payload) {
      payload.scheduledAt satisfies Date
      return { scheduleId: payload.scheduleId }
    },
  })
  scheduled.id satisfies "scheduled"
  const scheduledInput: TaskInput<typeof scheduled> = {
    scheduledAt: "2026-08-06T00:00:00Z",
    timezone: "UTC",
    scheduleId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
    upcoming: [],
  }
  scheduledInput.scheduledAt satisfies string
  const scheduledOutput: TaskOutput<typeof scheduled> = {
    scheduleId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
  }
  scheduledOutput.scheduleId satisfies string

  task({
    id: "duration",
    maxDuration: "15m",
    ttl: "7d",
    retry: {
      maxAttempts: 2,
      backoff: { minDelay: "1s", maxDelay: "30s", factor: 2 },
    },
    run: () => null,
  })
  task({
    id: "invalid-duration",
    // @ts-expect-error public durations never accept bare numbers.
    maxDuration: 900,
    run: () => null,
  })

  const operator: Actor = actor({
    id: "operator",
    async run(session, ctx) {
      ctx satisfies ActorContext
      const sessionID: string = session.id
      const actorID: string = ctx.actor.id
      void sessionID
      void actorID
      const input = await session.input.receive()
      session.input.receive() satisfies ActorSessionReceive
      if (input.ok) await session.output.append(input.value)
      // @ts-expect-error only a Session ref input has send().
      session.input.send(null)
      // @ts-expect-error only a Session ref retrieves the durable Session resource.
      session.retrieve()
      // @ts-expect-error only a Session ref closes the durable Session resource.
      session.close()
      // @ts-expect-error Actor output is authored rather than remotely listed.
      session.output.list()
      // @ts-expect-error Actor output has no definition-level schema.
      session.output.schema
      // @ts-expect-error Session addressing is not copied into Actor definition identity.
      ctx.actor.key
      // @ts-expect-error Actor definition identity is not copied onto the Session.
      session.actorId
    },
  })
  // @ts-expect-error Actor values must be created by actor().
  const unbrandedActor: Actor = {
    id: "unbranded",
    start: async () => null as never,
  }
  void unbrandedActor
  operator.start({ workspace: runtimeWorkspace }).then((started) => {
    started satisfies ActorStartResult
    const { run } = started
    client.runs.wait(run).unwrap().then((output) => {
      output satisfies null
    })
  })
  client.actors.start("operator", { workspace: clientWorkspace }).then(({ run }) => {
    client.runs.wait(run).unwrap().then((output) => {
      output satisfies null
    })
  })
}
