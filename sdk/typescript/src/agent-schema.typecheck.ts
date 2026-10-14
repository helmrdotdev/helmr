import { agent, type AgentInput, type AgentResult, type StandardSchemaV1, type TemplateDefinition, type Turn } from "./agent"
declare const template: TemplateDefinition
declare const inputSchema: StandardSchemaV1<{ count: string }, { count: number }>
declare const resultSchema: StandardSchemaV1<{ at: Date }, { at: string }>
const worker = agent({
  id: "worker", template, inputSchema, resultSchema,
  init: () => ({ connection: { run: (n: number) => n * 2 } }),
  turn(turn, ctx) {
    const count: number = ctx.initResult.connection.run(turn.input.count)
    // @ts-expect-error handler input is transformed
    const text: string = turn.input.count
    void count; void text
    return { at: new Date() }
  },
  triggers: [{ id: "nightly", cron: "0 0 * * *", timezone: "UTC", input: { count: "2" } }],
})
const id: "worker" = worker.id
const input: AgentInput<typeof worker> = { count: "2" }
const result: AgentResult<typeof worker> = { at: "now" }
void id; void input; void result
// @ts-expect-error caller input is not transformed
const wrongInput: AgentInput<typeof worker> = { count: 2 }
// @ts-expect-error stored result has been transformed
const wrongResult: AgentResult<typeof worker> = { at: new Date() }
void wrongInput; void wrongResult
agent({ id: "simple", template, turn(turn, ctx) {
  // @ts-expect-error no initializer cannot invent state
  ctx.initResult.connection.run(1)
  return turn.input
} })
agent({ id: "annotated", template,
  // @ts-expect-error annotations cannot manufacture validated input
  turn: (turn: Turn<{ count: number }>) => turn.input.count,
})
agent({ id: "bad-cron", template, inputSchema, turn: turn => turn.input.count,
  // @ts-expect-error cron cannot widen the schema's accepted input
  triggers: [{ id: "nightly", cron: "0 0 * * *", timezone: "UTC", input: { count: 2 } }],
})
declare const dateResult: StandardSchemaV1<unknown, Date>
// @ts-expect-error result schema must produce JSON
agent({ id: "date", template, resultSchema: dateResult, turn: () => new Date() })

// @ts-expect-error explicit schema type cannot invent a missing validator
agent<"absent", typeof inputSchema>({ id: "absent", template, turn: t => t.input.count })
// @ts-expect-error explicit result schema cannot invent a missing transform
agent<"absent", undefined, typeof resultSchema>({ id: "absent", template, turn: () => ({ at: new Date() }) })
// @ts-expect-error explicit state cannot invent initialization
const missingInit: import("./agent").AgentDefinition<import("./content").InputContent, number, { value: number }> = { kind: "agent", id: "x", template, turn: (_, c) => c.initResult.value }
// @ts-expect-error narrowed input requires a real schema
const missingSchema: import("./agent").AgentDefinition<{count:number}, number> = { kind: "agent", id: "x", template, turn: t => t.input.count }
void missingInit; void missingSchema
interface Wire { count: string; nested?: { title: string }; items: readonly string[] }
interface Result { text: string; nested?: Wire }
declare const interfaces: StandardSchemaV1<Wire, { count: number }>
declare const resultInterfaces: StandardSchemaV1<Date, Result>
declare const wire: Wire
const interfaceAgent = agent({ id: "interfaces", template, inputSchema: interfaces, resultSchema: resultInterfaces, turn: () => new Date(), triggers: [{ id: "tick", cron: "* * * * *", timezone: "UTC", input: wire }] })
const acceptedInterface: AgentInput<typeof interfaceAgent> = wire
void acceptedInterface

async function typedCalls(client: import("./client").HelmrClient) {
  const receipt = await client.sessions.create<typeof interfaceAgent>({ agent: "interfaces", input: wire })
  const next = await receipt.session.enqueue(wire)
  const result = (await next.get()).result
  const check: Result | undefined = result
  void check
  // @ts-expect-error invocation uses accepted input, not parsed input
  await receipt.session.enqueue({ count: 1, items: [] })
  // @ts-expect-error ID is part of the imported contract
  await client.sessions.create<typeof interfaceAgent>({ agent: "wrong", input: wire })
  const empty = await client.sessions.create<typeof interfaceAgent>({ agent: "interfaces" })
  const none: null = empty.turn
  void none
  const attached = await client.sessions.attach<typeof interfaceAgent>({ id: "session", agent: "interfaces", deploymentId: "deployment" })
  await attached.enqueue(wire)
  // @ts-expect-error unverified refs cannot acquire a result type
  client.sessions.ref<typeof interfaceAgent>("session")
}
void typedCalls

// @ts-expect-error direct definitions cannot declare a known non-JSON result
const invalidDefinition: import("./agent").AgentDefinition<import("./content").InputContent, Date> = { kind: "agent", id: "x", template, turn: () => new Date() }
void invalidDefinition
declare const optionalDefinition: import("./agent").AgentContract<"optional", string | undefined, string>
async function optionalCreate(client: import("./client").HelmrClient) {
  const empty = await client.sessions.create<typeof optionalDefinition>({ agent: "optional", input: undefined })
  // @ts-expect-error undefined input cannot promise an initial Turn
  empty.turn.id
}
void optionalCreate

agent({ id: "explicit-json-result", template, async turn(turn): Promise<import("./contract").JsonValue> {
  const parts: string[] = turn.input.map(part => part.text)
  // @ts-expect-error conversational input cannot be any
  const impossible: number = turn.input
  void impossible
  return parts
} })
