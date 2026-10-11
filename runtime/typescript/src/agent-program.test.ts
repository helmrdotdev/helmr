import type { InputContent } from "../../../sdk/typescript/src/content"
import assert from "node:assert/strict"
import test from "node:test"
import { create, fromBinary, toBinary } from "@bufbuild/protobuf"
import { agentProto } from "@helmr/proto"
import type { AgentDefinition, Json, TurnOutcome } from "../../../sdk/typescript/src/agent"
import { runAgentProgram } from "./agent-program"

function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>(yes => { resolve = yes }); return { promise, resolve } }
function queue<T>() {
  const values: T[] = []
  let wake = deferred<void>()
  let ended = false
  return {
    push(value: T) { values.push(value); wake.resolve() },
    end() { ended = true; wake.resolve() },
    async *[Symbol.asyncIterator]() { for (;;) { if (values.length) { yield values.shift()!; continue }; if (ended) return; wake = deferred<void>(); await wake.promise } },
  }
}
const identity = create(agentProto.SessionIdentitySchema, { sessionId: "session", processEpoch: 3n })
const json = (value: unknown) => Buffer.from(JSON.stringify(value))
function fixture(definition: Pick<AgentDefinition<InputContent, Json, unknown>, "turn" | "setup">, handle?: (operation: agentProto.Operation) => Promise<Json>, terminalSequence = 0n, beforeWrite?: (event: agentProto.ProgramEvent["event"]) => void) {
  const input = queue<Uint8Array>()
  const ready = deferred<void>(), failed = deferred<agentProto.SessionFailed>()
  const receipts = new Map<string, ReturnType<typeof deferred<agentProto.DeliveryResult>>>()
  const operations: agentProto.Operation[] = []
  const cancellations: string[] = []
  const checkpoints = new Map<string, ReturnType<typeof deferred<agentProto.SessionCheckpointReady>>>()
  function send(command: agentProto.GuestCommand['command'], deliveryId = "", controlSequence = 0n) {
    const body = toBinary(agentProto.GuestCommandSchema, create(agentProto.GuestCommandSchema, { identity, deliveryId, controlSequence, command }))
    const header = Buffer.alloc(4); header.writeUInt32BE(body.length)
    // Exercise fragmented frame headers and payloads.
    input.push(header.subarray(0, 2)); input.push(Buffer.concat([header.subarray(2), body.subarray(0, 3)])); input.push(body.subarray(3))
  }
  async function operate(operation: agentProto.Operation): Promise<Json> {
    if (handle) return handle(operation)
    const value = JSON.parse(Buffer.from(operation.payloadJson).toString())
    if (operation.method === agentProto.Operation_Method.FINALIZE) return { status: "completed", result: value.result }
    if (operation.method === agentProto.Operation_Method.FAIL) return { status: value.error.code === "interrupted" ? "interrupted" : "failed", error: value.error }
    return null
  }
  const running = runAgentProgram({ kind: "agent", id: "agent", computer: undefined!, ...definition }, {
    input,
    write: async frame => {
      const event = fromBinary(agentProto.ProgramEventSchema, frame.subarray(4)).event
      beforeWrite?.(event)
      if (event.case === "cancelOperation") cancellations.push(event.value.requestId)
      if (event.case === "ready") ready.resolve()
      if (event.case === "checkpointReady") checkpoints.get(event.value.checkpointId)!.resolve(event.value)
      if (event.case === "failed") failed.resolve(event.value)
      if (event.case === "deliveryResult") receipts.get(event.value.deliveryId)!.resolve(event.value)
      if (event.case === "operation") {
        operations.push(event.value)
        void operate(event.value).then(value => send({ case: "operationResult", value: create(agentProto.OperationResultSchema, { requestId: event.value.requestId, outcome: { case: "valueJson", value: json(value) } }) }))
      }
    },
  })
  send({ case: "start", value: create(agentProto.SessionStartSchema, { agentId: "agent", computerId: "computer", deploymentId: "deployment", recoveryKind: agentProto.SessionStart_RecoveryKind.INITIAL, parentSessionId: "parent", terminalSequence }) })
  let sequence = 0
  function command(value: agentProto.GuestCommand['command'], controlSequence?: bigint) {
    const id = `delivery-${++sequence}`, receipt = deferred<agentProto.DeliveryResult>()
    receipts.set(id, receipt)
    send(value, id, controlSequence ?? BigInt(sequence))
    return receipt.promise
  }
  const dispatch = (turn: number) => command({ case: "dispatch", value: create(agentProto.TurnDispatchSchema, { turnId: `turn-${turn}`, sequence: BigInt(turn), createdAt: "2026-10-04T00:00:00Z", inputJson: json([{ type: "text", text: String(turn) }]), sourceJson: json({ kind: "api" }) }) })
  const shutdown = async () => { await command({ case: "shutdown", value: create(agentProto.SessionShutdownSchema, { reason: "test finished" }) }); input.end(); await running }
  const checkpoint = (checkpointId: string) => {
    const result = deferred<agentProto.SessionCheckpointReady>()
    checkpoints.set(checkpointId, result)
    send({ case: "checkpoint", value: create(agentProto.SessionCheckpointSchema, { checkpointId }) })
    return result.promise
  }
  return { checkpoint, ready: ready.promise, failed: failed.promise, command, dispatch, operations, cancellations, shutdown, running, input }
}
function value(receipt: agentProto.DeliveryResult): TurnOutcome {
  assert.equal(receipt.outcome.case, "valueJson")
  return receipt.outcome.case === "valueJson" ? JSON.parse(Buffer.from(receipt.outcome.value).toString()) : assert.fail("delivery failed")
}

test("framed Session program retains setup and completes operation responses while dispatch waits", async () => {
  let setups = 0
  const f = fixture({ setup: () => { setups++; return { count: 0 } }, turn: (_turn, ctx) => ++(ctx.setupResult as { count: number }).count })
  await f.ready
  assert.deepEqual(value(await f.dispatch(1)), { status: "completed", result: 1 })
  assert.deepEqual(value(await f.dispatch(2)), { status: "completed", result: 2 })
  assert.equal(setups, 1)
  assert.deepEqual(f.operations.map(operation => operation.method), [agentProto.Operation_Method.CLOSE_PROCESSING, agentProto.Operation_Method.CONVERGE_NATIVE, agentProto.Operation_Method.FINALIZE, agentProto.Operation_Method.CLOSE_PROCESSING, agentProto.Operation_Method.CONVERGE_NATIVE, agentProto.Operation_Method.FINALIZE])
  await f.shutdown()
})

test("idle suspension prevents dispatch until authorized resume and watermark blocks retired input", async () => {
  const f = fixture({ turn: turn => turn.input }, undefined, 4n)
  await f.ready
  const stale = await f.dispatch(4)
  assert.equal(stale.outcome.case, "error")
  await f.command({ case: "suspend", value: create(agentProto.SessionSuspendSchema, { reason: "human hold" }) })
  assert.equal((await f.dispatch(5)).outcome.case, "error")
  await f.command({ case: "resume", value: create(agentProto.SessionResumeSchema) })
  assert.deepEqual(value(await f.dispatch(5)).result, [{ type: "text", text: "5" }])
  await f.shutdown()
})

test("setup failure reports hold and never becomes ready on a resume command", async () => {
  let setups = 0
  const f = fixture({ setup: () => { setups++; throw new Error("setup broke") }, turn: () => assert.fail("unexpected Turn") })
  assert.match((await f.failed).message, /setup broke/)
  assert.equal(f.operations[0]?.method, agentProto.Operation_Method.HOLD)
  assert.equal((await f.command({ case: "resume", value: create(agentProto.SessionResumeSchema) })).outcome.case, "error")
  assert.equal((await f.dispatch(1)).outcome.case, "error")
  assert.equal(setups, 1)
  await f.shutdown()
})

test("output pipe failure wakes a silent input reader and rejects the program", async () => {
  const start = create(agentProto.GuestCommandSchema, { identity, command: { case: "start", value: create(agentProto.SessionStartSchema, { agentId: "agent", computerId: "computer", deploymentId: "deployment", recoveryKind: agentProto.SessionStart_RecoveryKind.INITIAL }) } })
  const body = toBinary(agentProto.GuestCommandSchema, start)
  const header = Buffer.alloc(4); header.writeUInt32BE(body.length)
  const input = queue<Uint8Array>()
  input.push(Buffer.concat([header, body]))
  const running = runAgentProgram({ kind: "agent", id: "agent", computer: undefined!, turn: () => null }, { input, write: async () => { throw new Error("output pipe lost") } })
  await assert.rejects(running, /output pipe lost/)
  input.end()
})

test("unexpected EOF is process loss, not a clean shutdown", async () => {
  const f = fixture({ turn: () => null })
  await f.ready
  f.input.end()
  await assert.rejects(f.running, /closed unexpectedly/)
})


test("parent context contains only identity and Turn exposes no control facade", async () => {
  const f = fixture({ setup: async ctx => {
    assert.deepEqual(Object.keys(ctx.session.parent!), ["id"])
    assert.equal(ctx.session.parent!.id, "parent")
    return null
  }, turn: turn => { assert.equal(Object.hasOwn(turn, "agents"), false); return null } })
  await f.ready
  assert.equal(f.operations.length, 0)
  assert.equal(value(await f.dispatch(1)).status, "completed")
  await f.shutdown()
})


test("operation write failure cancels a handler that catches the write error", async () => {
  const cancelled = deferred<void>()
  const f = fixture({ turn: async turn => {
    try { await turn.output.write("value") } catch {}
    if (!turn.signal.aborted) await new Promise<void>(resolve => turn.signal.addEventListener("abort", () => resolve(), { once: true }))
    cancelled.resolve()
    return null
  } }, undefined, 0n, event => {
    if (event.case === "operation" && event.value.method === agentProto.Operation_Method.OUTPUT) throw new Error("operation pipe lost")
  })
  await f.ready
  void f.dispatch(1)
  await assert.rejects(f.running, /operation pipe lost/)
  await cancelled.promise
  f.input.end()
})

test("a delayed resume cannot release a newer suspension", async () => {
  let invocations = 0
  const f = fixture({ turn: () => ++invocations })
  await f.ready
  await f.command({ case: "suspend", value: create(agentProto.SessionSuspendSchema, { reason: "hold A" }) }, 1n)
  await f.command({ case: "resume", value: create(agentProto.SessionResumeSchema) }, 2n)
  await f.command({ case: "suspend", value: create(agentProto.SessionSuspendSchema, { reason: "hold B" }) }, 3n)
  assert.deepEqual(value(await f.command({ case: "resume", value: create(agentProto.SessionResumeSchema) }, 2n)), { superseded: true })
  assert.equal((await f.dispatch(1)).outcome.case, "error")
  assert.equal(invocations, 0)
  await f.command({ case: "resume", value: create(agentProto.SessionResumeSchema) }, 4n)
  assert.equal(value(await f.dispatch(1)).result, 1)
  await f.shutdown()
})

test("bundle loader failure emits a Session failure before closing the local pipe", async () => {
  const input = queue<Uint8Array>()
  const events: agentProto.ProgramEvent[] = []
  const running = runAgentProgram(async () => { throw new Error("selected Agent export missing") }, {
    input, write: async frame => { events.push(fromBinary(agentProto.ProgramEventSchema, frame.subarray(4))) },
  })
  const body = toBinary(agentProto.GuestCommandSchema, create(agentProto.GuestCommandSchema, { identity, command: { case: "start", value: create(agentProto.SessionStartSchema, { agentId: "agent", computerId: "computer", deploymentId: "deployment", recoveryKind: agentProto.SessionStart_RecoveryKind.INITIAL }) } }))
  const header = Buffer.alloc(4); header.writeUInt32BE(body.length)
  input.push(Buffer.concat([header, body]))
  await running
  assert.equal(events.length, 1)
  assert.equal(events[0]!.event.case, "failed")
  if (events[0]!.event.case === "failed") assert.match(events[0]!.event.value.message, /selected Agent export missing/)
})


test("checkpoint observes idle continuation without releasing a hold or repeating setup", async () => {
  let setups = 0
  const f = fixture({ setup: () => { setups++; return { count: 0 } }, turn: (_turn, ctx) => ++(ctx.setupResult as { count: number }).count })
  await f.ready
  assert.equal((await f.checkpoint("initial")).outcome.case, "scopesJson")
  assert.equal(value(await f.dispatch(1)).result, 1)
  await f.command({ case: "suspend", value: create(agentProto.SessionSuspendSchema, { reason: "held" }) })
  assert.equal((await f.checkpoint("held")).outcome.case, "scopesJson")
  assert.equal((await f.dispatch(2)).outcome.case, "error")
  await f.command({ case: "resume", value: create(agentProto.SessionResumeSchema) })
  assert.equal(value(await f.dispatch(2)).result, 2)
  assert.equal(setups, 1)
  await f.shutdown()
})

test("checkpoint rejects an active Turn without interrupting its work", async () => {
  const entered = deferred<void>(), release = deferred<void>()
  const f = fixture({ turn: async () => { entered.resolve(); await release.promise; return 7 } })
  await f.ready
  const turn = f.dispatch(1)
  await entered.promise
  assert.equal((await f.checkpoint("busy")).outcome.case, "error")
  release.resolve()
  assert.equal(value(await turn).result, 7)
  assert.equal((await f.checkpoint("idle")).outcome.case, "scopesJson")
  await f.shutdown()
})

test("an authored background timer pins compute even without an active Turn", async () => {
  let timer: ReturnType<typeof setInterval> | undefined
  const f = fixture({ setup: () => { timer = setInterval(() => {}, 1000); return null }, turn: () => null })
  try {
    await f.ready
    const blocked = await f.checkpoint("background")
    assert.equal(blocked.outcome.case, "error")
    if (blocked.outcome.case === "error") assert.match(blocked.outcome.value.message, /Timeout/)
    clearInterval(timer)
    // async_hooks destruction follows the event-loop turn that closes a handle.
    await new Promise<void>(resolve => setImmediate(resolve))
    assert.equal((await f.checkpoint("drained")).outcome.case, "scopesJson")
  } finally { clearInterval(timer); await f.shutdown() }
})

test("interrupted Turn cleanup callbacks pin a held Session until their work ends", async () => {
  const entered = deferred<void>(), stopped = deferred<void>()
  let timer: ReturnType<typeof setInterval> | undefined
  const f = fixture({ turn: async turn => {
    turn.signal.addEventListener("abort", () => { timer = setInterval(() => {}, 1000); stopped.resolve() }, { once: true })
    entered.resolve()
    await stopped.promise
    return null
  } })
  try {
    await f.ready
    const pending = f.dispatch(1)
    await entered.promise
    await f.command({ case: "interrupt", value: create(agentProto.TurnInterruptSchema, { turnId: "turn-1", reason: "test" }) })
    assert.equal(value(await pending).status, "interrupted")
    const checkpoint = await f.checkpoint("held")
    assert.equal(checkpoint.outcome.case, "error")
    if (checkpoint.outcome.case === "error") assert.match(checkpoint.outcome.value.message, /Timeout/)
    clearInterval(timer)
    assert.equal((await f.checkpoint("drained")).outcome.case, "scopesJson")
  } finally { clearInterval(timer); await f.shutdown() }
})

test("managed MCP factory works during setup and is removed at process shutdown", async () => {
  const { createRuntimeMcpConnection } = await import("../../../sdk/typescript/src/mcp")
  await assert.rejects(createRuntimeMcpConnection(), /managed Session/)
  const connection = { url: "http://127.0.0.1:31415/mcp", headers: { Authorization: "Bearer local-only" } }
  let setups = 0
  const f = fixture({ setup: async () => { setups++; return createRuntimeMcpConnection() }, turn: (_turn, ctx) => (ctx.setupResult as typeof connection).url }, async operation => {
    const payload = JSON.parse(Buffer.from(operation.payloadJson).toString())
    if (operation.method === agentProto.Operation_Method.GET_MCP_CONNECTION) {
      assert.equal(operation.turnId, undefined)
      assert.equal(payload, null)
      return connection
    }
    if (operation.method === agentProto.Operation_Method.FINALIZE) return { status: "completed", result: payload.result }
    return null
  })
  await f.ready
  assert.equal(value(await f.dispatch(1)).result, connection.url)
  assert.equal(value(await f.dispatch(2)).result, connection.url)
  assert.equal(setups, 1)
  await f.shutdown()
  await assert.rejects(createRuntimeMcpConnection(), /managed Session/)
})

test("resume waits for interrupted work and a later hold supersedes the wait", async () => {
  for (const supersede of [false, true]) {
    const started = deferred<void>(), drain = deferred<void>()
    const f = fixture({ turn: async turn => {
      if (turn.sequence === 1) { started.resolve(); await drain.promise }
      return turn.sequence
    } })
    await f.ready
    const first = f.dispatch(1)
    await started.promise
    await f.command({ case: "suspend", value: create(agentProto.SessionSuspendSchema, { reason: "interrupt" }) }, 10n)
    const resume = f.command({ case: "resume", value: create(agentProto.SessionResumeSchema) }, 11n)
    if (supersede) await f.command({ case: "suspend", value: create(agentProto.SessionSuspendSchema, { reason: "later hold" }) }, 12n)
    drain.resolve()
    assert.equal(value(await first).status, "interrupted")
    assert.deepEqual(value(await resume), supersede ? { superseded: true } : null)
    if (supersede) {
      assert.equal((await f.dispatch(2)).outcome.case, "error")
      await f.command({ case: "resume", value: create(agentProto.SessionResumeSchema) }, 13n)
    }
    assert.equal(value(await f.dispatch(2)).result, 2)
    await f.shutdown()
  }
})
