import type { InputContent } from "../../../sdk/typescript/src/content"
import type { AskAnswer } from "../../../sdk/typescript/src/question"
import type { HumanContent } from "../../../sdk/typescript/src/content"
import assert from "node:assert/strict"
import test from "node:test"
import type { AgentDefinition, OutputReceipt, InitContext, Turn, TurnOutcome } from "../../../sdk/typescript/src/agent"
import type { JsonValue } from "../../../sdk/typescript/src/contract"
import { MessageRejected } from "../../../sdk/typescript/src/message-error"
import { OperationError } from "./agent-channel"
import { MessageBusy, SessionRuntime, type SessionDriver, type TurnTiming } from "./agent-session"
import { parseAsk } from "../../../sdk/typescript/src/internal/asks"

function deferred<T>() { let resolve!: (value: T) => void, reject!: (error: unknown) => void; const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no }); return { promise, resolve, reject } }
const question = (text: string) => ({ prompt: [{ type: "text" as const, text }],  type: "text" as const })
const reply = (answer: JsonValue): AskAnswer => ({ answer, answeredBy: { kind: "user", id: "answerer" } })
const context: InitContext = { session: { id: "session" }, computer: { id: "computer" }, deployment: { id: "deployment" }, recovery: { kind: "initial" }, mcp: { getConnectionOptions: async () => { throw new Error("MCP is not configured in this fixture") } }, signal: new AbortController().signal }
const textInput = (text: string): InputContent => [{ type: "text", text }]
const request = (sequence = 1) => ({ id: `turn-${sequence}`, sequence, createdAt: "2026-10-04T00:00:00Z", input: textInput(String(sequence)), source: { kind: "api_key" as const } })
function fixture(definition: Pick<AgentDefinition<InputContent, JsonValue, unknown>, "turn"> & Partial<Pick<AgentDefinition<InputContent, JsonValue, unknown>, "init">> & { inputSchema?: import("../../../sdk/typescript/src/agent").StandardSchemaV1; resultSchema?: import("../../../sdk/typescript/src/agent").StandardSchemaV1<unknown, JsonValue> }, overrides: Partial<SessionDriver> = {}, limits: NonNullable<ConstructorParameters<typeof SessionRuntime>[3]> = { failureConvergenceMs: 100 }) {
  const calls: string[] = []
  const driver: SessionDriver = {
    registerMessages: async () => { calls.push("register") },
    closeProcessing: async () => { calls.push("close") },
    ask: async () => { throw new Error("unused ask") },
    respond: async () => { calls.push("respond") },
    output: async () => { calls.push("output"); return { sequence: 1 } },
    confirmDrain: async () => { calls.push("drained") },
    finalize: async (_id, result) => { calls.push("finalize"); return { status: "completed", result } },
    fail: async (_id, error) => { calls.push("fail", `failure:${error.message}`); return error.code === "interrupted" ? { status: "interrupted" } : { status: "failed", error } },
    hold: async reason => { calls.push(`hold:${reason}`) },
    ...overrides,
  }
  const runtime = new SessionRuntime({ kind: "agent", id: "agent", template: undefined!, ...definition }, context, driver, limits)
  return { runtime, calls, driver }
}

test("response staging follows received progress and drains before processing closes", async () => {
  const emitted = deferred<void>(), release = deferred<OutputReceipt>()
  const operations: string[] = []
  const { runtime } = fixture({ turn: turn => { void turn.output.write("progress"); void turn.respond(""); return 42 } }, {
    output: async () => { operations.push("output"); emitted.resolve(); return release.promise },
    respond: async (_turn, _id, content) => { assert.deepEqual(content, [{ type: "text", text: "" }]); operations.push("response") },
    closeProcessing: async () => { operations.push("close") },
  })
  const outcome = runtime.dispatch(request())
  await emitted.promise
  assert.deepEqual(operations, ["output"])
  release.resolve({ sequence: 1 })
  assert.equal((await outcome).status, "completed")
  assert.deepEqual(operations, ["output", "response", "close"])
})

test("caught progress budget rejection permits a separate final response", async () => {
  let staged = false
  const { runtime } = fixture({ turn: async turn => {
    await assert.rejects(turn.output.write("too much"), /progress limit/)
    await turn.respond("bounded response")
    return true
  } }, { output: async () => { throw new OperationError("content_limit_exceeded", "progress limit") }, respond: async () => { staged = true } })
  assert.equal((await runtime.dispatch(request())).status, "completed")
  assert.equal(staged, true)
})

test("setup result retains identity and memory across turns", async () => {
  let setups = 0, seen: unknown
  const { runtime } = fixture({ init: () => { setups++; return { counter: 0 } }, turn: (_turn, ctx) => {
    if (seen) assert.equal(ctx.initResult, seen)
    seen = ctx.initResult
    return ++(ctx.initResult as { counter: number }).counter
  } })
  assert.deepEqual(await runtime.dispatch(request()), { status: "completed", result: 1 })
  assert.deepEqual(await runtime.dispatch(request(2)), { status: "completed", result: 2 })
  assert.equal(setups, 1)
  assert.equal(runtime.idle, true)
})

test("duplicate delivery and uncertain publication never rerun a handler", async () => {
  let invocations = 0, publications = 0
  const { runtime } = fixture({ turn: () => { invocations++; return "done" } }, { finalize: async (_id, result) => { if (++publications === 1) throw new Error("reply lost"); return { status: "completed", result } } })
  const original = runtime.dispatch(request())
  assert.equal(runtime.dispatch(request()), original)
  await assert.rejects(original, /reply lost/)
  await assert.rejects(runtime.dispatch(request(2)), /cannot dispatch/)
  assert.equal(runtime.idle, false)
  assert.deepEqual(await runtime.reconcileFinalization("turn-1"), { status: "completed", result: "done" })
  assert.equal(invocations, 1)
})

test("settlement timing includes drainage and reconciliation without exposing authored payloads", async t => {
  let clock = 10, attempts = 0
  t.mock.method(performance, "now", () => clock)
  const draining = deferred<void>(), release = deferred<void>()
  const records: TurnTiming[] = []
  const { runtime } = fixture({ turn: () => ({ private: "not diagnostic content" }) }, {
    closeProcessing: async () => { draining.resolve(); await release.promise },
    finalize: async (_id, result) => {
      if (++attempts === 1) throw new Error("reply lost")
      clock = 100
      return { status: "completed", result }
    },
  }, { failureConvergenceMs: 100, onTiming: record => { records.push(record); throw new Error("diagnostic unavailable") } })
  const initial = runtime.dispatch(request())
  await draining.promise
  clock = 30
  release.resolve()
  await assert.rejects(initial, /reply lost/)
  assert.deepEqual(records, [], "uncertain finalization is not a settled observation")
  clock = 90
  assert.equal((await runtime.reconcileFinalization("turn-1")).status, "completed")
  assert.equal((await runtime.dispatch(request())).status, "completed")
  assert.deepEqual(records, [{
    sessionId: "session", turnId: "turn-1", sequence: 1, outcome: "completed",
    handlerReturnToSettlementMs: 90, handlerReturnToFinalizationMs: 20,
    finalizationToSettlementMs: 70,
  }])
})

test("a failed handler has no fabricated return timing", async () => {
  const records: TurnTiming[] = []
  const { runtime } = fixture({ turn: () => { throw new Error("handler failed") } }, {}, {
    failureConvergenceMs: 100, onTiming: record => records.push(record),
  })
  assert.equal((await runtime.dispatch(request())).status, "failed")
  assert.deepEqual(records, [])
})

test("failed setup holds queued work and is not implicitly retried", async () => {
  let setups = 0
  const { runtime, calls } = fixture({ init: () => { setups++; throw new Error("broken bundle setup") }, turn: () => assert.fail("handler ran") })
  await assert.rejects(runtime.dispatch(request()), /broken bundle setup/)
  await assert.rejects(runtime.initialize(), /broken bundle setup/)
  await assert.rejects(runtime.dispatch(request()), /cannot dispatch/)
  assert.throws(() => runtime.resume(), /reconstruction/)
  assert.equal(setups, 1)
  assert.deepEqual(calls, ["hold:setup_failed"])
})

test("reconstructed runtime rejects deliveries through the durable terminal watermark", async () => {
  let invocations = 0
  const { runtime } = fixture({ turn: () => ++invocations }, {}, { failureConvergenceMs: 100, terminalSequence: 4 })
  await assert.rejects(runtime.dispatch(request(4)), /acknowledged/)
  await assert.rejects(runtime.dispatch(request(1)), /acknowledged/)
  assert.equal((await runtime.dispatch(request(5))).result, 1)
  assert.equal(invocations, 1)
  for (const terminalSequence of [-1, 1.5, Number.MAX_SAFE_INTEGER + 1]) {
    assert.throws(() => fixture({ turn: () => null }, {}, { failureConvergenceMs: 100, terminalSequence }), /terminal sequence/)
  }
})

test("reconciling an already running unheld Session does not interrupt work", async () => {
  const started = deferred<void>(), finish = deferred<void>()
  const { runtime } = fixture({ turn: async () => { started.resolve(); await finish.promise; return "done" } })
  const outcome = runtime.dispatch(request())
  await started.promise
  runtime.resume()
  finish.resolve()
  assert.equal((await outcome).status, "completed")
})

test("authorized resume admits new input only after interruption settles", async () => {
  const started = deferred<void>()
  let setups = 0
  const { runtime } = fixture({ init: () => ++setups, turn: async turn => {
    if (turn.sequence === 1) {
      started.resolve()
      await new Promise<void>(resolve => turn.signal.addEventListener("abort", () => resolve(), { once: true }))
    }
    return turn.sequence
  } })
  const outcome = runtime.dispatch(request())
  await started.promise
  runtime.suspend(new Error("Session held"))
  const stopped = runtime.interrupt("turn-1", new Error("human interrupt"))
  assert.throws(() => runtime.resume(), /reconciliation/)
  assert.equal((await outcome).status, "interrupted")
  await stopped
  await assert.rejects(runtime.dispatch(request(2)), /cannot dispatch/)
  runtime.resume()
  assert.equal((await runtime.dispatch(request(2))).result, 2)
  assert.equal((await runtime.dispatch(request())).status, "interrupted")
  assert.equal(setups, 1)
})

test("unproved native convergence cannot resume the surviving process", async () => {
  const { runtime } = fixture({ turn: () => { throw new Error("handler failed") } }, { confirmDrain: async () => { throw new Error("native still running") } })
  await assert.rejects(runtime.dispatch(request()), /native still running/)
  assert.throws(() => runtime.resume(), /reconstruction/)
  await assert.rejects(runtime.dispatch(request(2)), /cannot dispatch/)
})

test("authored question identity can be read by the external SDK", async () => {
  const { runtime } = fixture({ turn: async turn => (await turn.ask(question("permission"))).answer }, {
    ask: async (_turn, creationId, question) => {
      const resource = parseAsk({ id: creationId,
        session_id: "01900000-0000-7000-8000-000000000001", turn_id: "01900000-0000-7000-8000-000000000002",
        status: "pending", created_at: "2026-10-07T00:00:00Z", question })
      assert.equal(resource.id, creationId)
      return { answer: Promise.resolve(reply("yes")), withdraw: async () => assert.fail("answered question withdrawn") }
    },
  })
  assert.deepEqual(await runtime.dispatch(request()), { status: "completed", result: "yes" })
})

test("forgotten question is withdrawn and cannot become success", async () => {
  const answer = deferred<AskAnswer>(); let withdrawals = 0
  const { runtime, calls } = fixture({ turn: turn => { void turn.ask(question("permission")); return 1 } }, { ask: async () => ({ answer: answer.promise, withdraw: async () => { withdrawals++; answer.reject(new Error("withdrawn")) } }) })
  assert.equal((await runtime.dispatch(request())).status, "failed")
  assert.ok(withdrawals >= 1)
  assert.ok(!calls.includes("finalize"))
  assert.ok(calls.includes("drained"))
})

test("already-started callback drains its question and output after return", async () => {
  const handlerReady = deferred<void>(), returnHandler = deferred<void>(), answer = deferred<AskAnswer>(), asked = deferred<void>()
  let callbackTurn: Turn | undefined
  const { runtime, calls } = fixture({ turn: async turn => {
    callbackTurn = turn
    await turn.onMessage(async () => { await turn.ask(question("question")); await turn.output.write("callback output"); assert.throws(() => turn.ask(question("late question")), /closed/) })
    handlerReady.resolve(); await returnHandler.promise; return 1
  } }, { ask: async () => { asked.resolve(); return { answer: answer.promise, withdraw: async () => answer.reject(new Error("withdrawn")) } } })
  const outcome = runtime.dispatch(request())
  await handlerReady.promise
  const callback = runtime.deliverMessage("turn-1", "message-1", textInput("follow-up"), { kind: "user" })
  await asked.promise
  returnHandler.resolve()
  await new Promise<void>(resolve => setImmediate(resolve))
  assert.ok(!calls.includes("finalize"))
  assert.throws(() => callbackTurn!.ask(question("another new question")), /closed/)
  answer.resolve(reply("approved"))
  await callback
  assert.equal((await outcome).status, "completed")
  assert.ok(calls.indexOf("output") < calls.indexOf("finalize"))
})

test("an admitted output pipe must finish even if the handler forgets to await it", async () => {
  const produce = deferred<void>()
  async function* stream() { await produce.promise; yield "later output" }
  const { runtime, calls } = fixture({ turn: turn => { void turn.output.pipe(stream()); return 1 } })
  const outcome = runtime.dispatch(request())
  await new Promise<void>(resolve => setImmediate(resolve))
  assert.ok(!calls.includes("finalize"))
  produce.resolve()
  assert.equal((await outcome).status, "completed")
  assert.ok(calls.indexOf("output") < calls.indexOf("finalize"))
})

test("forgotten rejected output fails the turn", async () => {
  const { runtime, calls } = fixture({ turn: turn => { void turn.output.write([{ type: "json", value: 1 }]); return 1 } }, { output: async () => { throw new Error("output not durable") } })
  assert.equal((await runtime.dispatch(request())).status, "failed")
  assert.ok(!calls.includes("finalize"))
})

test("failed native convergence holds the Session instead of dispatching a peer turn", async () => {
  const { runtime, calls } = fixture({ turn: () => { throw new Error("application failure") } }, { confirmDrain: async () => { throw new Error("child still running") } })
  await assert.rejects(runtime.dispatch(request()), /child still running/)
  await assert.rejects(runtime.dispatch(request(2)), /cannot dispatch/)
  assert.ok(calls.includes("hold:turn_drain_failed"))
  assert.ok(!calls.includes("fail"))
})

test("retained Turn output and message registration reject after the handler boundary", async () => {
  let oldTurn: Turn | undefined
  const { runtime } = fixture({ turn: turn => { oldTurn = turn; return 1 } })
  await runtime.dispatch(request())
  assert.throws(() => oldTurn!.respond("late"), /closed/)
  assert.throws(() => oldTurn!.onMessage(() => {}), /closed/)
})

test("completed callback scope cannot admit detached output during native convergence", async () => {
  const ready = deferred<void>(), returnHandler = deferred<void>(), releaseDetached = deferred<void>(), converging = deferred<void>(), nativeDone = deferred<void>()
  let detached: Promise<unknown> | undefined
  const { runtime, calls } = fixture({ turn: async turn => {
    await turn.onMessage(() => { detached = releaseDetached.promise.then(() => turn.output.write("too late")); void detached.catch(() => {}) })
    ready.resolve(); await returnHandler.promise; return 1
  } }, { confirmDrain: async () => { converging.resolve(); await nativeDone.promise } })
  const result = runtime.dispatch(request())
  await ready.promise
  await runtime.deliverMessage("turn-1", "message-1", textInput("start callback"), { kind: "user" })
  returnHandler.resolve()
  await converging.promise
  releaseDetached.resolve()
  await assert.rejects(detached!, /output is closed/)
  assert.ok(!calls.includes("output"))
  nativeDone.resolve()
  assert.equal((await result).status, "completed")
})

test("callback failure remains part of drainage after its delivery response", async () => {
  const ready = deferred<void>(), returnHandler = deferred<void>()
  const { runtime, calls } = fixture({ turn: async turn => {
    await turn.onMessage(() => { throw new Error("callback failed") })
    ready.resolve(); await returnHandler.promise; return 1
  } })
  const result = runtime.dispatch(request())
  await ready.promise
  await assert.rejects(runtime.deliverMessage("turn-1", "message-1", textInput("input"), { kind: "user" }), /callback failed/)
  returnHandler.resolve()
  assert.equal((await result).status, "failed")
  assert.ok(!calls.includes("finalize"))
})

test("customer input mutation does not change transport delivery identity", async () => {
  let invocations = 0
  const { runtime } = fixture({ turn: turn => { invocations++; (turn.input[0] as { text: string }).text = "mutated"; return turn.input } })
  const delivered = { ...request(), input: [{ type: "text" as const, text: "original" }] }
  const result = runtime.dispatch(delivered)
  await result
  assert.equal(delivered.input[0]!.text, "original")
  assert.equal(runtime.dispatch({ ...request(), input: [{ text: "original", type: "text" }] }), result)
  await assert.rejects(runtime.dispatch({ ...request(), input: textInput("changed") }), /identity changed/)
  assert.equal(invocations, 1)
})

test("duplicate message identity cannot replace the admitted payload", async () => {
  const ready = deferred<void>(), returnHandler = deferred<void>()
  let calls = 0
  const { runtime } = fixture({ turn: async turn => {
    await turn.onMessage(() => { calls++ })
    ready.resolve(); await returnHandler.promise; return 1
  } })
  const result = runtime.dispatch(request())
  await ready.promise
  const delivered = runtime.deliverMessage("turn-1", "message-1", [{ type: "text", text: "original" }], { kind: "user" })
  await delivered
  assert.equal(runtime.deliverMessage("turn-1", "message-1", [{ text: "original", type: "text" }], { kind: "user" }), delivered)
  await assert.rejects(runtime.deliverMessage("turn-1", "message-1", textInput("changed"), { kind: "user" }), /identity changed/)
  assert.equal(calls, 1)
  returnHandler.resolve()
  await result
})

test("lost failure settlement reply reconciles without rerunning processing or convergence", async () => {
  let attempts = 0, handlers = 0
  const { runtime, calls } = fixture({ turn: () => { handlers++; throw new Error("application failure") } }, { fail: async (_id, error) => {
    if (++attempts === 1) throw new Error("settlement reply lost")
    return { status: "failed", error }
  } })
  await assert.rejects(runtime.dispatch(request()), /settlement reply lost/)
  assert.equal(runtime.idle, false)
  assert.ok(!calls.some(call => call.startsWith("hold:")))
  assert.equal((await runtime.reconcileFinalization("turn-1")).status, "failed")
  assert.equal(handlers, 1)
  assert.equal(calls.filter(call => call === "drained").length, 1)
  assert.equal(runtime.idle, true)
})

test("caught ask registration failures allow an application fallback", async () => {
  const { runtime } = fixture({ turn: async turn => {
    await assert.rejects(turn.ask(question("question")), /registration unavailable/)
    return "fallback"
  } }, { ask: async () => { throw new Error("registration unavailable") } })
  assert.deepEqual(await runtime.dispatch(request()), { status: "completed", result: "fallback" })
})

test("caller cancellation of a pipe can be handled without failing the Turn", async () => {
  const next = deferred<IteratorResult<HumanContent>>()
  let returned = 0
  const { runtime } = fixture({ turn: async turn => {
    const cancel = new AbortController()
    const stream: AsyncIterable<HumanContent> = { [Symbol.asyncIterator]: () => ({ next: () => next.promise, return: async () => { returned++; return { done: true, value: undefined } } }) }
    const pipe = turn.output.pipe(stream, { signal: cancel.signal })
    cancel.abort(new Error("application cancelled pipe"))
    await assert.rejects(pipe, /application cancelled pipe/)
    return "fallback"
  } })
  assert.equal((await runtime.dispatch(request())).status, "completed")
  assert.equal(returned, 1)
})

test("failure convergence holds when a callback ignores interruption", async () => {
  const ready = deferred<void>(), callbackStarted = deferred<void>(), returnHandler = deferred<void>(), callbackEnd = deferred<void>()
  const { runtime, calls } = fixture({ turn: async turn => {
    await turn.onMessage(async () => { callbackStarted.resolve(); await callbackEnd.promise })
    ready.resolve(); await returnHandler.promise; return 1
  } }, {}, { failureConvergenceMs: 20 })
  const result = runtime.dispatch(request())
  void result.catch(() => {})
  await ready.promise
  const message = runtime.deliverMessage("turn-1", "message-1", textInput("1"), { kind: "user" })
  await callbackStarted.promise
  returnHandler.resolve()
  await new Promise<void>(resolve => setImmediate(resolve))
  const stopRejected = assert.rejects(runtime.interrupt("turn-1", new Error("operator interrupt")), /convergence timed out/)
  await assert.rejects(result, /convergence timed out/)
  await stopRejected
  assert.ok(calls.includes("hold:turn_drain_failed"))
  assert.ok(!calls.includes("finalize"))
  await assert.rejects(runtime.dispatch(request(2)), /cannot dispatch/)
  callbackEnd.resolve(); await message
})

test("exact interruption settles without adding a hold", async () => {
  const ready = deferred<void>()
  const { runtime } = fixture({ turn: turn => new Promise((_resolve, reject) => {
    turn.signal.addEventListener("abort", () => reject(turn.signal.reason), { once: true }); ready.resolve()
  }) })
  const result = runtime.dispatch(request())
  await ready.promise
  await runtime.interrupt("turn-1", new Error("operator interrupted"))
  const outcome = await result
  assert.equal(outcome.status, "interrupted")
  assert.equal(outcome.error, undefined)
  assert.equal(runtime.idle, true)
})

test("failure withdraws only outstanding asks exactly once even when answer never settles", async () => {
  let withdrawals = 0
  const { runtime } = fixture({ turn: async turn => {
    assert.deepEqual(await turn.ask(question("answered")), reply("yes"))
    void turn.ask(question("pending"))
    await new Promise<void>(resolve => setImmediate(resolve))
    throw new Error("application failed")
  } }, { ask: async (_id, _creation, question) => ({ answer: question.prompt[0]?.type === "text" && question.prompt[0].text === "answered" ? Promise.resolve(reply("yes")) : new Promise(() => {}), withdraw: async () => { if (question.prompt[0]?.type === "text" && question.prompt[0].text === "answered") assert.fail("answered question withdrawn"); withdrawals++ } }) })
  assert.equal((await runtime.dispatch(request())).status, "failed")
  assert.equal(withdrawals, 1)
})

test("message backpressure is distinguishable and the exact payload can be retried", async () => {
  const ready = deferred<void>(), release = deferred<void>(), returnHandler = deferred<void>()
  const received: JsonValue[] = []
  const { runtime } = fixture({ turn: async turn => {
    await turn.onMessage(async value => { received.push(value.content); if (value.content[0]?.text === "1") await release.promise })
    ready.resolve(); await returnHandler.promise; return 1
  } })
  const result = runtime.dispatch(request())
  await ready.promise
  const first = runtime.deliverMessage("turn-1", "one", textInput("1"), { kind: "user" })
  await assert.rejects(runtime.deliverMessage("turn-1", "two", textInput("2"), { kind: "user" }), MessageBusy)
  release.resolve(); await first
  await runtime.deliverMessage("turn-1", "two", textInput("2"), { kind: "user" })
  assert.deepEqual(received, [textInput("1"), textInput("2")])
  returnHandler.resolve(); await result
})

test("concurrent settlement reconciliation reuses one in-flight operation", async () => {
  const started = deferred<void>(), finish = deferred<TurnOutcome>()
  let settlements = 0
  const { runtime } = fixture({ turn: () => 1 }, { finalize: async () => { settlements++; started.resolve(); return finish.promise } })
  const original = runtime.dispatch(request())
  await started.promise
  const a = runtime.reconcileFinalization("turn-1"), b = runtime.reconcileFinalization("turn-1")
  assert.equal(a, b)
  finish.resolve({ status: "completed", result: 1 })
  await Promise.all([original, a, b])
  assert.equal(settlements, 1)
})

test("terminal receipt retirement prevents delayed delivery from rerunning processing", async () => {
  let invocations = 0
  const { runtime } = fixture({ turn: () => ++invocations })
  await runtime.dispatch(request())
  runtime.acknowledgeTerminal("turn-1", 1)
  runtime.acknowledgeTerminal("turn-1", 1)
  await assert.rejects(runtime.dispatch(request()), /already been acknowledged/)
  await assert.rejects(runtime.dispatch({ ...request(), id: "another-old-id" }), /already been acknowledged/)
  assert.equal((await runtime.dispatch(request(2))).result, 2)
  assert.equal(invocations, 2)
})

test("a draining pipe's extra writes join before final settlement", async () => {
  const begin = deferred<void>(), wrote = deferred<void>(), acknowledgment = deferred<OutputReceipt>()
  const { runtime, calls } = fixture({ turn: turn => {
    async function* stream() { await begin.promise; void turn.output.write("extra"); wrote.resolve(); yield "ordinary" }
    void turn.output.pipe(stream())
    return 1
  } }, { output: async (_turn, _id, value) => value[0]?.type === "text" && value[0].text === "extra" ? acknowledgment.promise : { sequence: 2 } })
  const result = runtime.dispatch(request())
  await new Promise<void>(resolve => setImmediate(resolve))
  begin.resolve(); await wrote.promise
  await new Promise<void>(resolve => setImmediate(resolve))
  assert.ok(!calls.includes("finalize"))
  acknowledgment.resolve({ sequence: 1 })
  assert.equal((await result).status, "completed")
})

test("interrupt immediately closes message admission before asynchronous cleanup", async () => {
  const ready = deferred<void>()
  let callbacks = 0
  const { runtime } = fixture({ turn: async turn => {
    await turn.onMessage(() => { callbacks++ })
    return new Promise((_resolve, reject) => { turn.signal.addEventListener("abort", () => reject(turn.signal.reason), { once: true }); ready.resolve() })
  } })
  const result = runtime.dispatch(request())
  await ready.promise
  runtime.interrupt("turn-1", new Error("stop now"))
  await assert.rejects(runtime.deliverMessage("turn-1", "late", textInput("1"), { kind: "user" }), /stop now/)
  assert.equal((await result).status, "interrupted")
  assert.equal(callbacks, 0)
})

test("uncertain finalization retains a snapshot of the returned JSON", async () => {
  const authored = { value: 1 }
  const observed: JsonValue[] = []
  const { runtime } = fixture({ turn: () => authored }, { finalize: async (_id, result) => {
    observed.push(structuredClone(result))
    if (observed.length === 1) throw new Error("reply lost")
    return { status: "completed", result }
  } })
  await assert.rejects(runtime.dispatch(request()), /reply lost/)
  authored.value = 2
  assert.equal((await runtime.reconcileFinalization("turn-1")).status, "completed")
  assert.deepEqual(observed, [{ value: 1 }, { value: 1 }])
})


test("blocked generator cancellation holds instead of hanging or settling", async () => {
  const idle = deferred<void>(), entered = deferred<void>()
  const own = new AbortController()
  async function* values() { entered.resolve(); await idle.promise; yield "one" }
  const { runtime, calls } = fixture({ turn: async turn => {
    const pipe = turn.output.pipe(values(), { signal: own.signal })
    await entered.promise
    own.abort(new Error("pipe cancelled"))
    await pipe.catch(() => {})
    return null
  } }, {}, { failureConvergenceMs: 20 })
  await assert.rejects(runtime.dispatch(request()), /convergence timed out/)
  assert.ok(calls.includes("hold:turn_drain_failed"))
  assert.ok(!calls.includes("finalize"))
  idle.resolve()
})

test("interrupt stops native work before waiting for its handler", async () => {
  const entered = deferred<void>(), stopped = deferred<void>()
  const { runtime, calls } = fixture({ turn: async turn => { turn.onCancel(() => { stopped.resolve() }); entered.resolve(); await stopped.promise; return null } })
  const pending = runtime.dispatch(request())
  await entered.promise
  runtime.interrupt("turn-1", new Error("stop"))
  assert.equal((await pending).status, "interrupted")
  assert.ok(!calls.includes("hold:turn_drain_failed"))
})

test("new deliveries cannot regress the dispatched sequence before acknowledgment", async () => {
  const { runtime } = fixture({ turn: async () => null })
  await runtime.dispatch(request(5))
  await assert.rejects(runtime.dispatch(request(2)), /invalid sequence/)
})


test("failure attestation follows cancellation and authored drainage", async () => {
  const entered = deferred<void>(), stopped = deferred<void>()
  const order: string[] = []
  const { runtime } = fixture({ turn: async turn => {
    turn.onCancel(() => { order.push("cancel"); stopped.resolve() })
    entered.resolve(); await stopped.promise
    order.push("handler joined")
    return null
  } }, {
    confirmDrain: async () => { order.push("drained") },
    fail: async (_id, error) => { order.push("settle"); return { status: "interrupted" } },
  })
  const pending = runtime.dispatch(request())
  await entered.promise
  runtime.interrupt("turn-1", new Error("stop"))
  await pending
  assert.deepEqual(order, ["cancel", "handler joined", "drained", "settle"])
})


test("known steering rejection remains visible without failing the Turn", async () => {
  const ready = deferred<void>(), finish = deferred<void>()
  const { runtime } = fixture({ turn: async turn => {
    await turn.onMessage(() => { throw new MessageRejected("unsupported steering") })
    ready.resolve(); await finish.promise; return "done"
  } })
  const pending = runtime.dispatch(request())
  await ready.promise
  await assert.rejects(runtime.deliverMessage("turn-1", "message", [], { kind: "user" }), /unsupported steering/)
  finish.resolve()
  assert.equal((await pending).status, "completed")
})

test("hold receipt failure retains the original setup and convergence diagnosis", async () => {
  const setup = fixture({ init: () => { throw new Error("setup broke") }, turn: () => null }, { hold: async () => { throw new Error("upstream renewing") } })
  await assert.rejects(setup.runtime.initialize(), error => error instanceof AggregateError && error.errors[0].message === "setup broke" && error.errors[1].message === "upstream renewing" && error.message.includes("setup broke"))
  const native = fixture({ turn: () => { throw new Error("handler broke") } }, { confirmDrain: async () => { throw new Error("native still running") }, hold: async () => { throw new Error("hold reply lost") } })
  await assert.rejects(native.runtime.dispatch(request()), error => error instanceof AggregateError && error.errors[0].message === "native still running" && error.errors[1].message === "hold reply lost")
  assert.throws(() => native.runtime.resume(), /reconstruction/)
})


test("the handler joins native callbacks and their output before settlement", async () => {
  const admitted = deferred<void>(), write = deferred<void>()
  const { runtime, calls } = fixture({ turn: async turn => {
    admitted.resolve()
    await write.promise
    await turn.output.write("native output before handler return")
    return "returned"
  } })
  const pending = runtime.dispatch(request())
  await admitted.promise
  assert.equal(calls.includes("finalize"), false)
  write.resolve()
  assert.equal((await pending).status, "completed")
  assert.ok(calls.indexOf("output") < calls.indexOf("finalize"))
})

test("lost native continuation still joins authored callbacks before interrupted settlement", async () => {
  const started = deferred<void>(), callbackDone = deferred<void>(), handlerDone = deferred<void>()
  const { runtime, calls } = fixture({ turn: async turn => {
    turn.onCancel(() => { throw new Error("native continuation lost") })
    await turn.onMessage(async () => { started.resolve(); await callbackDone.promise })
    await handlerDone.promise
    throw new Error("application failure before cleanup loss")
  } })
  const pending = runtime.dispatch(request())
  await new Promise(resolve => setImmediate(resolve))
  const callback = runtime.deliverMessage("turn-1", "message", [], { kind: "user" })
  await started.promise
  handlerDone.resolve()
  await new Promise(resolve => setImmediate(resolve))
  assert.equal(calls.includes("fail"), false)
  callbackDone.resolve()
  await callback
  const outcome = await pending
  assert.equal(outcome.status, "interrupted")
  assert.ok(calls.includes("failure:native continuation lost"))
  assert.ok(calls.includes("hold:session_continuation_lost"))
  assert.equal(runtime.idle, false)
  assert.throws(() => runtime.resume(), /reconstruction/)
})

test("message callbacks retain their own immutable Slack source", async () => {
  const ready = deferred<void>(), done = deferred<void>()
  let received: unknown
  const initialSource = { kind: "user" as const, slack: { teamId: "T", channelId: "C", threadTs: "1.0", messageTs: "1.0", userId: "first" } }
  const { runtime } = fixture({ turn: async turn => {
    await turn.onMessage(message => { received = message; assert.equal(turn.source.slack?.userId, "first") })
    ready.resolve(); await done.promise; return 1
  } })
  const result = runtime.dispatch({ ...request(), source: initialSource })
  await ready.promise
  const source = { kind: "user" as const, slack: { ...initialSource.slack, messageTs: "2.0", userId: "second" } }
  const callback = runtime.deliverMessage("turn-1", "message", textInput("hello"), source)
  source.slack.userId = "mutated"
  await callback
  assert.deepEqual(received, { id: "message", content: textInput("hello"), source: { kind: "user", slack: { ...initialSource.slack, messageTs: "2.0", userId: "second" } } })
  await assert.rejects(runtime.deliverMessage("turn-1", "message", textInput("hello"), source), /identity changed/)
  done.resolve(); await result
})

for (const status of ["interrupted", "completed"] as const) {
  test(`interrupt joins provisional finalization with authoritative ${status} outcome`, async () => {
    const started = deferred<void>(), finish = deferred<TurnOutcome>()
    let settlements = 0
    const { runtime, calls } = fixture({ turn: () => 1 }, { finalize: async () => { settlements++; started.resolve(); return finish.promise } })
    const original = runtime.dispatch(request())
    await started.promise
    let acknowledged = false
    const stopped = runtime.interrupt("turn-1", new Error("durable stop")).then(() => { acknowledged = true })
    const checked = status === "completed" ? assert.rejects(stopped, /already recorded/) : stopped
    await Promise.resolve()
    assert.equal(acknowledged, false)
    finish.resolve(status === "completed" ? { status, result: 1 } : { status })
    await Promise.all([original, checked])
    assert.equal(settlements, 1)
    assert.equal(acknowledged, status === "interrupted")
    assert.equal(runtime.canCheckpoint, true)
    assert.equal(calls.some(call => call.startsWith("hold:")), false)
  })
}

test("cancellation hooks release the handler concurrently and are joined once", async () => {
  const started = deferred<void>(), releaseHandler = deferred<void>(), releaseHook = deferred<void>()
  let invoked = 0
  const { runtime, calls } = fixture({ turn: async turn => {
    turn.onCancel(async reason => { assert.equal(reason, "stop"); invoked++; releaseHandler.resolve(); await releaseHook.promise })
    started.resolve()
    await releaseHandler.promise
    return null
  } })
  const pending = runtime.dispatch(request())
  await started.promise
  const stop = runtime.interrupt("turn-1", "stop")
  await releaseHandler.promise
  assert.equal(invoked, 1)
  assert.ok(!calls.includes("fail"))
  releaseHook.resolve()
  await stop
  assert.equal((await pending).status, "interrupted")
  assert.equal(invoked, 1)
})

test("a cancellation hook registered during handler drainage also joins", async () => {
  const started = deferred<void>(), cancelled = deferred<void>(), hookStarted = deferred<void>(), finish = deferred<void>()
  const { runtime, calls } = fixture({ turn: async turn => {
    turn.signal.addEventListener("abort", () => cancelled.resolve(), { once: true })
    started.resolve()
    await cancelled.promise
    turn.onCancel(async () => { hookStarted.resolve(); await finish.promise })
    return null
  } })
  const pending = runtime.dispatch(request())
  await started.promise
  const stop = runtime.interrupt("turn-1", "stop")
  await hookStarted.promise
  assert.ok(!calls.includes("fail"))
  finish.resolve()
  await stop
  assert.equal((await pending).status, "interrupted")
})

test("handler failure invokes cancellation and rejected cleanup prevents reuse", async () => {
  let invoked = 0
  const { runtime, calls } = fixture({ turn: turn => {
    turn.onCancel(async () => { invoked++; throw new Error("native connection lost") })
    throw new Error("request failed")
  } })
  assert.equal((await runtime.dispatch(request())).status, "interrupted")
  assert.equal(invoked, 1)
  assert.ok(calls.includes("hold:session_continuation_lost"))
  await assert.rejects(runtime.dispatch(request(2)), /cannot dispatch/)
})

test("successful Turns discard cancellation hooks and reject late registration", async () => {
  let previous!: Turn, invoked = 0
  const { runtime } = fixture({ turn: turn => {
    previous = turn
    turn.onCancel(() => { invoked++ })
    return null
  } })
  assert.equal((await runtime.dispatch(request())).status, "completed")
  assert.equal(invoked, 0)
  assert.throws(() => previous.onCancel(() => {}), /registration is closed/)
  assert.equal((await runtime.dispatch(request(2))).status, "completed")
  assert.equal(invoked, 0)
})

test("a hanging cancellation hook holds without attesting or settling the Turn", async () => {
  const entered = deferred<void>()
  const { runtime, calls } = fixture({ turn: turn => {
    turn.onCancel(() => new Promise<void>(() => {}))
    entered.resolve()
    return new Promise<JsonValue>((_resolve, reject) => turn.signal.addEventListener("abort", () => reject(turn.signal.reason), { once: true }))
  } }, {}, { failureConvergenceMs: 20 })
  const pending = runtime.dispatch(request())
  await entered.promise
  const stop = runtime.interrupt("turn-1", new Error("stop"))
  await Promise.all([assert.rejects(pending, /convergence timed out/), assert.rejects(stop, /convergence timed out/)])
  assert.deepEqual(calls, ["hold:turn_drain_failed"])
  assert.throws(() => runtime.resume(), /reconstruction/)
})

test("an interruption winning finalization joins cancellation before its receipt", async () => {
  const entered = deferred<void>(), release = deferred<void>()
  let completed = false
  const { runtime } = fixture({ turn: turn => {
    turn.onCancel(async () => { entered.resolve(); await release.promise; completed = true })
    return "done"
  } }, { finalize: async () => ({ status: "interrupted" }) })
  const pending = runtime.dispatch(request())
  await entered.promise
  assert.equal(runtime.idle, false)
  release.resolve()
  assert.equal((await pending).status, "interrupted")
  assert.equal(completed, true)
  assert.equal(runtime.idle, true)
})

test("schemas validate after init and transform exactly once through settlement retry", async () => {
  const order: string[] = []
  let finalizations = 0
  const { runtime } = fixture({
    init: () => { order.push("init"); return { alive: true } },
    inputSchema: { "~standard": { version: 1, vendor: "test", validate: async value => {
      order.push("input"); return { value: { count: Number((value as { count: string }).count) } }
    } } },
    resultSchema: { "~standard": { version: 1, vendor: "test", validate: async value => {
      order.push("result"); return { value: { doubled: (value as { doubled: number }).doubled + 1 } }
    } } },
    turn: turn => { order.push("turn"); return { doubled: (turn.input as unknown as { count: number }).count * 2 } },
  }, { finalize: async (_id, result) => { if (++finalizations === 1) throw new Error("lost receipt"); return { status: "completed", result } } })
  await assert.rejects(runtime.dispatch({ ...request(), input: { count: "2" } }), /lost receipt/)
  assert.deepEqual(await runtime.reconcileFinalization("turn-1"), { status: "completed", result: { doubled: 5 } })
  assert.deepEqual(order, ["init", "input", "turn", "result"])
})

test("invalid input skips handler; invalid result cannot finalize staged response", async () => {
  for (const stage of ["input", "result"] as const) {
    let handled = false
    const schema = { "~standard": { version: 1 as const, vendor: "test", validate: () => ({ issues: [{ message: "bad", path: ["field"] }] }) } }
    const { runtime, calls } = fixture({
      ...(stage === "input" ? { inputSchema: schema } : { resultSchema: schema }),
      turn: async turn => { handled = true; await turn.respond("not publishable"); return 1 },
    })
    const outcome = await runtime.dispatch(request())
    assert.equal(outcome.status, "failed")
    assert.equal(outcome.error?.code, `${stage}_invalid`)
    assert.equal(handled, stage === "result")
    assert.equal(calls.includes("finalize"), false)
  }
})

test("cancellation joins asynchronous validation before native convergence settles", async () => {
  const entered = deferred<void>(), release = deferred<void>()
  let handled = false, ended = false
  const { runtime, calls } = fixture({
    inputSchema: { "~standard": { version: 1, vendor: "test", validate: async value => { entered.resolve(); await release.promise; ended = true; return { value } } } },
    turn: () => { handled = true; return 1 },
  })
  const pending = runtime.dispatch(request())
  await entered.promise
  const stop = runtime.interrupt("turn-1", new Error("stopped"))
  assert.equal(calls.includes("fail"), false)
  release.resolve()
  await stop
  assert.equal((await pending).status, "interrupted")
  assert.equal(ended, true)
  assert.equal(handled, false)
})

test("non-JSON result is result_invalid, while a throwing validator is handler_failed", async () => {
  for (const throwing of [false, true]) {
    const { runtime } = fixture({
      ...(throwing ? { resultSchema: { "~standard": { version: 1 as const, vendor: "test", validate: () => { throw new Error("validator crashed") } } } } : {}),
      turn: () => throwing ? null : new Date() as never,
    })
    const outcome = await runtime.dispatch(request())
    assert.equal(outcome.error?.code, throwing ? "handler_failed" : "result_invalid")
  }
})


test("default invocation rejects non-text input before the authored handler", async () => {
  let handled = false
  const { runtime, calls } = fixture({ turn: () => { handled = true; return null } })
  const outcome = await runtime.dispatch({ ...request(), input: [{ type: "file", path: "/private" }] })
  assert.equal(outcome.status, "failed")
  assert.equal(outcome.error?.code, "input_invalid")
  assert.equal(handled, false)
  assert.equal(calls.includes("finalize"), false)
})

test("live messages reject non-text content before the authored callback", async () => {
  const ready = deferred<void>(), release = deferred<void>()
  let callbacks = 0
  const { runtime } = fixture({ turn: async turn => {
    await turn.onMessage(() => { callbacks++ })
    ready.resolve(); await release.promise; return null
  } })
  const result = runtime.dispatch(request())
  await ready.promise
  try {
    await assert.rejects(runtime.deliverMessage("turn-1", "invalid", [{ type: "file", path: "/private" }] as unknown as InputContent, { kind: "user" }))
    assert.equal(callbacks, 0)
    await runtime.deliverMessage("turn-1", "valid", textInput("follow-up"), { kind: "user" })
    assert.equal(callbacks, 1)
  } finally { release.resolve(); await result }
})

test("held interruption retains its convergence bound through repeated controls", async () => {
  const started = deferred<void>(), release = deferred<void>(), aborted = deferred<void>()
  const { runtime, calls } = fixture({ turn: async turn => {
    turn.signal.addEventListener("abort", () => aborted.resolve(), { once: true })
    started.resolve(); await release.promise; return null
  } }, {}, { failureConvergenceMs: 40 })
  const pending = runtime.dispatch(request())
  const failed = assert.rejects(pending, /convergence timed out/)
  await started.promise
  runtime.suspend("capacity exhausted")
  const first = assert.rejects(runtime.interrupt("turn-1", new Error("storage stop")), /convergence timed out/)
  await aborted.promise
  runtime.suspend("still held")
  const retry = assert.rejects(runtime.interrupt("turn-1", new Error("repeated stop")), /convergence timed out/)
  try {
    await Promise.all([failed, first, retry])
    assert.equal(calls.filter(call => call === "hold:turn_drain_failed").length, 1)
    assert.throws(() => runtime.resume(), /reconciliation|reconstruction/)
  } finally { release.resolve() }
})

test("finalization interruption bounds cleanup even when a later hold arrives", async () => {
  const cleanupStarted = deferred<void>(), release = deferred<void>()
  const { runtime, calls } = fixture({ turn: turn => {
    turn.onCancel(async () => { cleanupStarted.resolve(); await release.promise })
    return null
  } }, { finalize: async () => ({ status: "interrupted" }) }, { failureConvergenceMs: 40 })
  const pending = runtime.dispatch(request())
  const failed = assert.rejects(pending, /convergence timed out/)
  await cleanupStarted.promise
  runtime.suspend("storage full")
  const stopped = assert.rejects(runtime.interrupt("turn-1", new Error("late stop")), /convergence timed out/)
  try {
    await Promise.all([failed, stopped])
    assert.ok(calls.includes("hold:turn_drain_failed"))
  } finally { release.resolve() }
})
