import {
  actor,
  image,
  source,
  task,
  workspace,
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
  const builder = workspace("machine")
  // @ts-expect-error image is required before resources.
  builder.resources({ cpu: 1, memory: "1GiB" })

  const resourceBuilder = builder.image(image("root").from("debian"))
  source.directory("./src")
  // @ts-expect-error v0 directory copy has no implementation-defined ignore language.
  source.directory("./src", { ignore: ["**/*.test.ts"] })
  // @ts-expect-error memory is required.
  resourceBuilder.resources({ cpu: 1 })
  // @ts-expect-error memory uses canonical MiB or GiB suffixes.
  resourceBuilder.resources({ cpu: 1, memory: "1Gi" })
  // @ts-expect-error v0 ephemeral disk capacity is not public input.
  resourceBuilder.resources({ cpu: 1, memory: "1GiB", disk: "64GiB" })

  const definition = resourceBuilder.resources({
    cpu: 1,
    memory: "1GiB",
  })
  definition.network({ internet: false })
  // @ts-expect-error denyCidrs is meaningless when internet is disabled.
  definition.network({ internet: false, denyCidrs: ["10.0.0.0/8"] })

  const payloadTask = task({
    id: "payload",
    payload: schema,
    run(payload): JsonValue {
      return payload
    },
  })
  payloadTask.start(
    { value: "ok" },
    { workspace: workspaces.ref({ key: "machine" }) },
  )
  const childWait = payloadTask.call(
    { value: "ok" },
    {
      workspace: workspaces.ref({ key: "machine" }),
      idempotencyKey: "payload:ok",
    },
  )
  childWait.unwrap().then((output) => {
    output satisfies JsonValue
  })
  payloadTask.call(
    { value: "ok" },
    // @ts-expect-error task.call requires an explicit idempotency key.
    { workspace: workspaces.ref({ key: "machine" }) },
  )
  // @ts-expect-error a payload-bearing task always requires payload.
  payloadTask.start({ workspace: workspaces.ref({ key: "machine" }) })
  const client = null as unknown as HelmrClient
  client.tasks.start<typeof payloadTask>("payload", {
    payload: { value: "ok" },
    workspace: workspaces.ref({ key: "machine" }),
  })
  // @ts-expect-error the external typed Task envelope requires payload.
  client.tasks.start<typeof payloadTask>("payload", {
    workspace: workspaces.ref({ key: "machine" }),
  })
  const typedOutputTask = task({
    id: "typed-output",
    payload: schema,
    run(payload) {
      return { value: payload.value, count: payload.value.length }
    },
  })
  const payloadTaskWait = client.runs.wait<typeof typedOutputTask>(
    "run_aaaaaaaaaaaaaaaaaaaaaaaaaa",
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
  noPayloadTask.start({ workspace: workspaces.ref({ key: "machine" }) })
  noPayloadTask.call({
    workspace: workspaces.ref({ key: "machine" }),
    idempotencyKey: "no-payload",
  })
  // @ts-expect-error task.call requires an explicit idempotency key.
  noPayloadTask.call({ workspace: workspaces.ref({ key: "machine" }) })
  client.tasks.start<typeof noPayloadTask>("no-payload", {
    workspace: workspaces.ref({ key: "machine" }),
  })
  client.tasks.start<typeof noPayloadTask>("no-payload", {
    // @ts-expect-error the external no-payload Task envelope forbids payload.
    payload: null,
    workspace: workspaces.ref({ key: "machine" }),
  })
  // @ts-expect-error a no-payload task has no payload position.
  noPayloadTask.start(null, { workspace: workspaces.ref({ key: "machine" }) })

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
    async run(self) {
      const input = await self.input.receive()
      if (input.ok) await self.output.append(input.value)
      // @ts-expect-error only ActorRef input has send().
      self.input.send(null)
      // @ts-expect-error Actor output has no definition-level schema.
      self.output.schema
    },
  })
}
