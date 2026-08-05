import {
  actor,
  image,
  secrets,
  source,
  task,
  sandbox,
  workspaces,
  type JsonValue,
  type HelmrClient,
  type PayloadSchema,
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
  const builder = sandbox({ id: "machine" })
  // @ts-expect-error image is required before resources.
  builder.resources({ cpu: 1, memory: "1GiB" })

  const resourceBuilder = builder.image(image("root").from("debian"))
  image("private").from("ghcr.io/acme/base:1", {
    auth: {
      username: "aktky",
      password: secrets.fromName("GHCR_TOKEN"),
    },
  })
  image("invalid-private").from("ghcr.io/acme/base:1", {
    auth: {
      username: "aktky",
      // @ts-expect-error registry passwords require a branded Secret name reference.
      password: "GHCR_TOKEN",
    },
  })
  source.directory("./src")
  // @ts-expect-error v0 directory copy has no implementation-defined ignore language.
  source.directory("./src", { ignore: ["**/*.test.ts"] })
  // @ts-expect-error memory is required.
  resourceBuilder.resources({ cpu: 1 })
  // @ts-expect-error memory uses canonical MiB or GiB suffixes.
  resourceBuilder.resources({ cpu: 1, memory: "1Gi" })
  // @ts-expect-error v0 ephemeral disk capacity is not public input.
  resourceBuilder.resources({ cpu: 1, memory: "1GiB", disk: "64GiB" })

  resourceBuilder.resources({
    cpu: 1,
    memory: "1GiB",
  })

  const payloadTask = task({
    id: "payload",
    payload: schema,
    run(payload): JsonValue {
      return payload
    },
  })
  const runtimeWorkspace = workspaces.ref(
    "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
  )
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
  // @ts-expect-error a payload-bearing task always requires payload.
  payloadTask.start({ workspace: runtimeWorkspace })
  const client = null as unknown as HelmrClient
  const clientWorkspace = client.workspaces.ref(
    "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
  )
  client.tasks.start<typeof payloadTask>("payload", {
    payload: { value: "ok" },
    workspace: clientWorkspace,
  })
  client.tasks.start<typeof payloadTask>("payload", {
    payload: { value: "ok" },
    // @ts-expect-error external client commands require a Workspace UUID.
    workspace: workspaces.fromKey("machine"),
  })
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
  const payloadTaskWait = client.runs.wait<typeof typedOutputTask>(
    "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
  )
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
      result.error.code satisfies string
    }
  })

  const noPayloadTask = task({
    id: "no-payload",
    run(): JsonValue {
      return null
    },
  })
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

  actor({
    id: "operator",
    async run(session, ctx) {
      const sessionID: string = session.id
      const actorID: string = ctx.actor.id
      void sessionID
      void actorID
      const input = await session.input.receive()
      if (input.ok) await session.output.append(input.value)
      // @ts-expect-error only a Session ref input has send().
      session.input.send(null)
      // @ts-expect-error Actor output has no definition-level schema.
      session.output.schema
      // @ts-expect-error Session addressing is not copied into Actor definition identity.
      ctx.actor.key
      // @ts-expect-error Actor definition identity is not copied onto the Session.
      session.actorId
    },
  })
}
