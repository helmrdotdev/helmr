import assert from "node:assert/strict"
import test from "node:test"
import { create, fromBinary } from "@bufbuild/protobuf"
import { agentProto } from "@helmr/proto"
import { AgentChannel, OperationError, OperationNotSent } from "./agent-channel"
const identity = create(agentProto.SessionIdentitySchema, { sessionId: "session", processEpoch: 4n })
const tick = () => new Promise<void>(resolve => setImmediate(resolve))
function fixture() {
  const frames: agentProto.ProgramEvent[] = []
  const channel = new AgentChannel(identity, async frame => {
    assert.equal(new DataView(frame.buffer, frame.byteOffset).getUint32(0), frame.length - 4)
    frames.push(fromBinary(agentProto.ProgramEventSchema, frame.subarray(4)))
  })
  return { channel, frames }
}
function requestId(frame: agentProto.ProgramEvent | undefined): string {
  assert.equal(frame?.event.case, "operation")
  return frame!.event.case === "operation" ? frame!.event.value.requestId : assert.fail("missing operation")
}
function result(id: string, value: unknown) { return create(agentProto.OperationResultSchema, { requestId: id, outcome: { case: "valueJson", value: Buffer.from(JSON.stringify(value)) } }) }

test("operation bytes preserve input and process identity across delayed results", async () => {
  const { channel, frames } = fixture()
  const payload = { input: 1 }
  const pending = channel.operation(agentProto.Operation_Method.OUTPUT, payload, { turnId: "turn" })
  payload.input = 2
  await tick()
  const id = requestId(frames[0])
  const operation = frames[0]!.event
  assert.equal(operation.case === "operation" && Buffer.from(operation.value.payloadJson).toString(), '{"input":1}')
  assert.deepEqual(frames[0]!.identity, identity)
  assert.throws(() => channel.receive(create(agentProto.SessionIdentitySchema, { sessionId: "session", processEpoch: 3n }), result(id, 7)), /different Session/)
  channel.receive(identity, result(id, 7))
  assert.equal(await pending, 7)
  channel.receive(identity, result(id, 8))
  assert.equal(frames.length, 1)
})

test("abort cancels observation after the original mutation frame without replay", async () => {
  const { channel, frames } = fixture()
  const abort = new AbortController()
  const pending = channel.operation(agentProto.Operation_Method.ASK, { question: "ok?" }, { signal: abort.signal })
  abort.abort(new Error("stopped waiting"))
  await assert.rejects(pending, /stopped waiting/)
  await tick()
  const id = requestId(frames[0])
  assert.equal(frames[1]?.event.case, "cancelOperation")
  assert.equal(frames[1]?.event.case === "cancelOperation" && frames[1].event.value.requestId, id)
  channel.receive(identity, result(id, "late receipt"))
  assert.equal(frames.length, 2)
})

test("local pipe failure rejects all outstanding operations and prevents later writes", async () => {
  let writes = 0
  const channel = new AgentChannel(identity, async () => { writes++; throw new Error("pipe lost") })
  const a = channel.operation(agentProto.Operation_Method.OUTPUT, 1)
  const b = channel.operation(agentProto.Operation_Method.FINALIZE, 2)
  await Promise.all([assert.rejects(a, /pipe lost/), assert.rejects(b, /pipe lost/)])
  await assert.rejects(channel.operation(agentProto.Operation_Method.FAIL, 3), /pipe lost/)
  assert.equal(writes, 1)
})

test("error and malformed responses reject their own operation", async () => {
  const { channel, frames } = fixture()
  const a = channel.operation(agentProto.Operation_Method.WAIT_TURN, null)
  const b = channel.operation(agentProto.Operation_Method.WAIT_ASK, null)
  await tick()
  channel.receive(identity, create(agentProto.OperationResultSchema, { requestId: requestId(frames[0]), outcome: { case: "error", value: { code: "held", message: "Session held" } } }))
  channel.receive(identity, create(agentProto.OperationResultSchema, { requestId: requestId(frames[1]), outcome: { case: "valueJson", value: Buffer.from("undefined") } }))
  await assert.rejects(a, error => error instanceof OperationError && error.code === "held")
  await assert.rejects(b, error => error instanceof OperationError && error.code === "invalid_response")
})

test("a never-sent oversized result settles failure and retries its lost receipt", async () => {
  const { GuestSessionDriver } = await import("./agent-driver")
  const { OperationNotSent } = await import("./agent-channel")
  const calls: { method: agentProto.Operation_Method; payload: unknown }[] = []
  let settlements = 0
  class RejectedChannel extends AgentChannel {
    override operation(method: agentProto.Operation_Method, payload: import("../../../sdk/typescript/src/agent").Json) {
      calls.push({ method, payload })
      if (method === agentProto.Operation_Method.FINALIZE) return Promise.reject(new OperationNotSent("request_too_large", "too large"))
      if (++settlements === 1) return Promise.reject(new Error("failure reply lost"))
      return Promise.resolve({ status: "failed", error: { code: "result_too_large", message: "Turn result exceeds the transport limit" } })
    }
  }
  const driver = new GuestSessionDriver(new RejectedChannel(identity, async () => assert.fail("unexpected transport write")))
  await assert.rejects(driver.finalize("turn", "result"), /failure reply lost/)
  assert.equal((await driver.finalize("turn", "result")).status, "failed")
  assert.deepEqual(calls.map(call => call.method), [agentProto.Operation_Method.FINALIZE, agentProto.Operation_Method.FAIL, agentProto.Operation_Method.FINALIZE, agentProto.Operation_Method.FAIL])
  assert.deepEqual(calls[1]?.payload, calls[3]?.payload)
  class RemoteRejection extends AgentChannel {
    override operation() { return Promise.reject(new OperationError("request_too_large", "guest rejected")) }
  }
  await assert.rejects(new GuestSessionDriver(new RemoteRejection(identity, async () => {})).finalize("turn", "result"), /guest rejected/)
})

test("escaped JSON beyond encoder capacity is never sent and leaves the channel usable", async () => {
  const { channel, frames } = fixture()
  // Repeated references keep the input small while its escaped JSON exceeds
  // both the frame bound and V8's maximum string length.
  const payload = { result: Array<string>(96).fill("\u0000".repeat(1024 * 1024)) }
  await assert.rejects(channel.operation(agentProto.Operation_Method.FINALIZE, payload),
    error => error instanceof OperationNotSent && error.code === "request_too_large")
  await tick()
  assert.equal(frames.length, 0)
  const pending = channel.operation(agentProto.Operation_Method.FAIL, { error: "result_too_large" })
  await tick()
  channel.receive(identity, result(requestId(frames[0]), { status: "failed" }))
  assert.deepEqual(await pending, { status: "failed" })
})

test("native scope proof is sent and accepted before classifying continuation loss", async () => {
  const { GuestSessionDriver } = await import("./agent-driver")
  const { SessionNativeRegistry, NativeContinuationLost } = await import("./agent-native")
  for (const rejected of [false, true]) {
    const { channel, frames } = fixture()
    const native = new SessionNativeRegistry(new AbortController().signal)
    native.evidence = async () => ({ scopes: [{ scopeId: "retained", state: "idle" }, { scopeId: "closed", state: "stopped" }], reusable: false })
    const driver = new GuestSessionDriver(channel, native)
    let finished = false
    const pending = driver.convergeNative("turn", "failed", AbortSignal.abort(new Error("handler failed")))
    void pending.then(() => { finished = true }, () => { finished = true })
    await tick()
    assert.equal(finished, false)
    assert.equal(frames[0]!.event.case, "operation")
    const operation = frames[0]!.event
    if (operation.case !== "operation") assert.fail("missing operation")
    assert.equal(operation.value.method, agentProto.Operation_Method.CONVERGE_NATIVE)
    assert.equal(operation.value.turnId, "turn")
    assert.deepEqual(JSON.parse(Buffer.from(operation.value.payloadJson).toString()), { disposition: "failed", scopes: [{ scopeId: "retained", state: "idle" }, { scopeId: "closed", state: "stopped" }] })
    if (rejected) {
      channel.receive(identity, create(agentProto.OperationResultSchema, { requestId: operation.value.requestId, outcome: { case: "error", value: { code: "unjoined", message: "Native scope still running" } } }))
      await assert.rejects(pending, error => error instanceof OperationError && !(error instanceof NativeContinuationLost))
    } else {
      channel.receive(identity, result(operation.value.requestId, null))
      await assert.rejects(pending, error => error instanceof NativeContinuationLost)
    }
  }
})

test("ask pending observations poll without occupying withdrawal transport", async () => {
  const { GuestSessionDriver } = await import("./agent-driver")
  let observations = 0, withdrawals = 0
  class AskChannel extends AgentChannel {
    override async operation(method: agentProto.Operation_Method, payload: import("../../../sdk/typescript/src/agent").Json) {
      const input = payload as { creationId?: string; askId?: string }
      if (method === agentProto.Operation_Method.ASK) return { id: input.creationId! }
      if (method === agentProto.Operation_Method.WITHDRAW_ASK) { withdrawals++; return null }
      assert.equal(method, agentProto.Operation_Method.WAIT_ASK)
      observations++
      return null
    }
  }
  const driver = new GuestSessionDriver(new AskChannel(identity, async () => assert.fail("unexpected frame")))
  const asks = await Promise.all(Array.from({ length: 65 }, (_, index) => driver.ask("turn", `ask-${index}`, { prompt: [], answer: { type: "text" } }, new AbortController().signal)))
  await tick()
  assert.equal(observations, 65)
  await Promise.all(asks.map(ask => ask.withdraw()))
  assert.equal(withdrawals, 65)
  const results = await Promise.allSettled(asks.map(ask => ask.answer))
  assert.ok(results.every(result => result.status === "rejected"))
  await new Promise(resolve => setTimeout(resolve, 270))
  assert.equal(observations, 65)
})

test("ask observation resolves exact empty text answer and authenticated identity", async () => {
  const { GuestSessionDriver } = await import("./agent-driver")
  let observations = 0
  class AnswerChannel extends AgentChannel {
    override async operation(method: agentProto.Operation_Method) {
      if (method === agentProto.Operation_Method.ASK) return { id: "ask" }
      assert.equal(method, agentProto.Operation_Method.WAIT_ASK)
      return ++observations === 1 ? null : { answer: "", respondedBy: { kind: "api_key", id: "key" } }
    }
  }
  const driver = new GuestSessionDriver(new AnswerChannel(identity, async () => assert.fail("unexpected frame")))
  const ask = await driver.ask("turn", "create", { prompt: [], answer: { type: "text" } }, new AbortController().signal)
  assert.deepEqual(await ask.answer, { answer: "", respondedBy: { kind: "api_key", id: "key" } })
  assert.equal(observations, 2)
})
