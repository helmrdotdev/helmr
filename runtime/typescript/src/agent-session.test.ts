import type { InputContent } from "../../../sdk/typescript/src/content"
import type { AskResponse } from "../../../sdk/typescript/src/question"
import type { HumanContent } from "../../../sdk/typescript/src/content"
import assert from "node:assert/strict"
import test from "node:test"
import type { AgentDefinition, Json, OutputReceipt, SetupContext, Turn, TurnOutcome } from "../../../sdk/typescript/src/agent"
import { NativeContinuationLost, SessionNativeRegistry } from "./agent-native"
import { MessageRejected } from "../../../sdk/typescript/src/message-error"
import { OperationError } from "./agent-channel"
import { MessageBusy, SessionRuntime, type SessionDriver, type TurnTiming } from "./agent-session"
import { parseAsk } from "../../../sdk/typescript/src/internal/asks"

function deferred<T>() { let resolve!: (value: T) => void, reject!: (error: unknown) => void; const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no }); return { promise, resolve, reject } }
const question = (text: string) => ({ prompt: [{ type: "text" as const, text }], answer: { type: "text" as const } })
const reply = (answer: Json): AskResponse => ({ answer, respondedBy: { kind: "user", id: "answerer" } })
const context: SetupContext = { session: { id: "session" }, computer: { id: "computer" }, deployment: { id: "deployment" }, recovery: { kind: "initial" }, signal: new AbortController().signal }
const textInput = (text: string): InputContent => [{ type: "text", text }]
const request = (sequence = 1) => ({ id: `turn-${sequence}`, sequence, createdAt: "2026-10-04T00:00:00Z", input: textInput(String(sequence)), source: { kind: "api" } })
function fixture(definition: Pick<AgentDefinition<InputContent, Json, unknown>, "turn" | "setup">, overrides: Partial<SessionDriver> = {}, limits: NonNullable<ConstructorParameters<typeof SessionRuntime>[3]> = { failureConvergenceMs: 100 }) {
  const calls: string[] = []
  const driver: SessionDriver = {
    registerMessages: async () => { calls.push("register") },
    closeProcessing: async () => { calls.push("close") },
    ask: async () => { throw new Error("unused ask") },
    respond: async () => { calls.push("respond") },
    output: async () => { calls.push("output"); return { sequence: 1 } },
    convergeNative: async (_id, outcome) => { calls.push(`converge:${outcome}`) },
    finalize: async (_id, result) => { calls.push("finalize"); return { status: "completed", result } },
    fail: async (_id, error) => { calls.push("fail"); return { status: error.code === "interrupted" ? "interrupted" : "failed", error } },
    hold: async reason => { calls.push(`hold:${reason}`) },
    ...overrides,
  }
  const runtime = new SessionRuntime({ kind: "agent", id: "agent", computer: undefined!, ...definition }, context, driver, limits)
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
  const { runtime } = fixture({ setup: () => { setups++; return { counter: 0 } }, turn: (_turn, ctx) => {
    if (seen) assert.equal(ctx.setupResult, seen)
    seen = ctx.setupResult
    return ++(ctx.setupResult as { counter: number }).counter
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
  const { runtime, calls } = fixture({ setup: () => { setups++; throw new Error("broken bundle setup") }, turn: () => assert.fail("handler ran") })
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
  const { runtime } = fixture({ setup: () => ++setups, turn: async turn => {
    if (turn.sequence === 1) {
      started.resolve()
      await new Promise<void>(resolve => turn.signal.addEventListener("abort", () => resolve(), { once: true }))
    }
    return turn.sequence
  } })
  const outcome = runtime.dispatch(request())
  await started.promise
  runtime.interrupt("turn-1", new Error("human interrupt"))
  assert.throws(() => runtime.resume(), /reconciliation/)
  assert.equal((await outcome).status, "interrupted")
  await assert.rejects(runtime.dispatch(request(2)), /cannot dispatch/)
  runtime.resume()
  assert.equal((await runtime.dispatch(request(2))).result, 2)
  assert.equal((await runtime.dispatch(request())).status, "interrupted")
  assert.equal(setups, 1)
})

test("unproved native convergence cannot resume the surviving process", async () => {
  const { runtime } = fixture({ turn: () => { throw new Error("handler failed") } }, { convergeNative: async () => { throw new Error("native still running") } })
  await assert.rejects(runtime.dispatch(request()), /native still running/)
  assert.throws(() => runtime.resume(), /reconstruction/)
  await assert.rejects(runtime.dispatch(request(2)), /cannot dispatch/)
})

test("authored question identity can be read by the external SDK", async () => {
  const { runtime } = fixture({ turn: async turn => (await turn.ask(question("permission"))).answer }, {
    ask: async (_turn, creationId, question) => {
      const resource = parseAsk({ id: creationId,
        session_id: "01900000-0000-7000-8000-000000000001", turn_id: "01900000-0000-7000-8000-000000000002",
        status: "pending", created_at: "2026-10-07T00:00:00Z", prompt: question.prompt, answer_control: question.answer })
      assert.equal(resource.id, creationId)
      return { answer: Promise.resolve(reply("yes")), withdraw: async () => assert.fail("answered question withdrawn") }
    },
  })
  assert.deepEqual(await runtime.dispatch(request()), { status: "completed", result: "yes" })
})

test("forgotten question is withdrawn and cannot become success", async () => {
  const answer = deferred<AskResponse>(); let withdrawals = 0
  const { runtime, calls } = fixture({ turn: turn => { void turn.ask(question("permission")); return 1 } }, { ask: async () => ({ answer: answer.promise, withdraw: async () => { withdrawals++; answer.reject(new Error("withdrawn")) } }) })
  assert.equal((await runtime.dispatch(request())).status, "failed")
  assert.ok(withdrawals >= 1)
  assert.ok(!calls.includes("finalize"))
  assert.ok(calls.includes("converge:failed"))
})

test("already-started callback drains its question and output after return", async () => {
  const handlerReady = deferred<void>(), returnHandler = deferred<void>(), answer = deferred<AskResponse>(), asked = deferred<void>()
  let callbackTurn: Turn | undefined
  const { runtime, calls } = fixture({ turn: async turn => {
    callbackTurn = turn
    await turn.onMessage(async () => { await turn.ask(question("question")); await turn.output.write("callback output"); assert.throws(() => turn.ask(question("late question")), /closed/) })
    handlerReady.resolve(); await returnHandler.promise; return 1
  } }, { ask: async () => { asked.resolve(); return { answer: answer.promise, withdraw: async () => answer.reject(new Error("withdrawn")) } } })
  const outcome = runtime.dispatch(request())
  await handlerReady.promise
  const callback = runtime.deliverMessage("turn-1", "message-1", textInput("follow-up"))
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
  const { runtime, calls } = fixture({ turn: () => { throw new Error("application failure") } }, { convergeNative: async (_id, _outcome, signal) => { assert.equal(signal.aborted, true); throw new Error("child still running") } })
  await assert.rejects(runtime.dispatch(request()), /child still running/)
  await assert.rejects(runtime.dispatch(request(2)), /cannot dispatch/)
  assert.ok(calls.includes("hold:native_convergence_failed"))
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
  } }, { convergeNative: async () => { converging.resolve(); await nativeDone.promise } })
  const result = runtime.dispatch(request())
  await ready.promise
  await runtime.deliverMessage("turn-1", "message-1", textInput("start callback"))
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
  await assert.rejects(runtime.deliverMessage("turn-1", "message-1", textInput("input")), /callback failed/)
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
  const delivered = runtime.deliverMessage("turn-1", "message-1", [{ type: "text", text: "original" }])
  await delivered
  assert.equal(runtime.deliverMessage("turn-1", "message-1", [{ text: "original", type: "text" }]), delivered)
  await assert.rejects(runtime.deliverMessage("turn-1", "message-1", textInput("changed")), /identity changed/)
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
  assert.equal(calls.filter(call => call === "converge:failed").length, 2)
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
  const message = runtime.deliverMessage("turn-1", "message-1", textInput("1"))
  await callbackStarted.promise
  returnHandler.resolve()
  await new Promise<void>(resolve => setImmediate(resolve))
  runtime.interrupt("turn-1", new Error("operator interrupt"))
  await assert.rejects(result, /convergence timed out/)
  assert.ok(calls.includes("hold:native_convergence_failed"))
  assert.ok(!calls.includes("finalize"))
  await assert.rejects(runtime.dispatch(request(2)), /cannot dispatch/)
  callbackEnd.resolve(); await message
})

test("interrupt settles explicitly as interrupted and keeps dispatch held", async () => {
  const ready = deferred<void>()
  const { runtime } = fixture({ turn: turn => new Promise((_resolve, reject) => {
    turn.signal.addEventListener("abort", () => reject(turn.signal.reason), { once: true }); ready.resolve()
  }) })
  const result = runtime.dispatch(request())
  await ready.promise
  runtime.interrupt("turn-1", new Error("operator interrupted"))
  const outcome = await result
  assert.equal(outcome.status, "interrupted")
  assert.equal(outcome.error?.code, "interrupted")
  assert.equal(runtime.idle, false)
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
  const received: Json[] = []
  const { runtime } = fixture({ turn: async turn => {
    await turn.onMessage(async value => { received.push(value); if (value[0]?.text === "1") await release.promise })
    ready.resolve(); await returnHandler.promise; return 1
  } })
  const result = runtime.dispatch(request())
  await ready.promise
  const first = runtime.deliverMessage("turn-1", "one", textInput("1"))
  await assert.rejects(runtime.deliverMessage("turn-1", "two", textInput("2")), MessageBusy)
  release.resolve(); await first
  await runtime.deliverMessage("turn-1", "two", textInput("2"))
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
  await assert.rejects(runtime.deliverMessage("turn-1", "late", textInput("1")), /stop now/)
  assert.equal((await result).status, "interrupted")
  assert.equal(callbacks, 0)
})

test("uncertain finalization retains a snapshot of the returned JSON", async () => {
  const authored = { value: 1 }
  const observed: Json[] = []
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
  assert.ok(calls.includes("hold:native_convergence_failed"))
  assert.ok(!calls.includes("finalize"))
  idle.resolve()
})

test("interrupt stops native work before waiting for its handler", async () => {
  const entered = deferred<void>(), stopped = deferred<void>()
  const { runtime, calls } = fixture({ turn: async () => { entered.resolve(); await stopped.promise; return null } }, {
    convergeNative: async (_id, outcome) => { assert.equal(outcome, "failed"); stopped.resolve() },
  })
  const pending = runtime.dispatch(request())
  await entered.promise
  runtime.interrupt("turn-1", new Error("stop"))
  assert.equal((await pending).status, "interrupted")
  assert.ok(!calls.includes("hold:native_convergence_failed"))
})

test("new deliveries cannot regress the dispatched sequence before acknowledgment", async () => {
  const { runtime } = fixture({ turn: async () => null })
  await runtime.dispatch(request(5))
  await assert.rejects(runtime.dispatch(request(2)), /invalid sequence/)
})


test("failure convergence verifies again after authored work joins", async () => {
  const entered = deferred<void>(), stopped = deferred<void>()
  const order: string[] = []
  const { runtime } = fixture({ turn: async () => {
    entered.resolve(); await stopped.promise
    order.push("handler joined")
    return null
  } }, {
    convergeNative: async () => { order.push("converge"); stopped.resolve() },
    fail: async (_id, error) => { order.push("settle"); return { status: "interrupted", error } },
  })
  const pending = runtime.dispatch(request())
  await entered.promise
  runtime.interrupt("turn-1", new Error("stop"))
  await pending
  assert.deepEqual(order, ["converge", "handler joined", "converge", "settle"])
})


test("known steering rejection remains visible without failing the Turn", async () => {
  const ready = deferred<void>(), finish = deferred<void>()
  const { runtime } = fixture({ turn: async turn => {
    await turn.onMessage(() => { throw new MessageRejected("unsupported steering") })
    ready.resolve(); await finish.promise; return "done"
  } })
  const pending = runtime.dispatch(request())
  await ready.promise
  await assert.rejects(runtime.deliverMessage("turn-1", "message", []), /unsupported steering/)
  finish.resolve()
  assert.equal((await pending).status, "completed")
})

test("hold receipt failure retains the original setup and convergence diagnosis", async () => {
  const setup = fixture({ setup: () => { throw new Error("setup broke") }, turn: () => null }, { hold: async () => { throw new Error("upstream renewing") } })
  await assert.rejects(setup.runtime.initialize(), error => error instanceof AggregateError && error.errors[0].message === "setup broke" && error.errors[1].message === "upstream renewing" && error.message.includes("setup broke"))
  const native = fixture({ turn: () => { throw new Error("handler broke") } }, { convergeNative: async () => { throw new Error("native still running") }, hold: async () => { throw new Error("hold reply lost") } })
  await assert.rejects(native.runtime.dispatch(request()), error => error instanceof AggregateError && error.errors[0].message === "native still running" && error.errors[1].message === "hold reply lost")
  assert.throws(() => native.runtime.resume(), /reconstruction/)
})


test("registered native output remains bound to its Turn while its producer drains", async () => {
  const native = new SessionNativeRegistry(context.signal)
  const initialized = Promise.resolve()
  let resource!: ReturnType<SessionNativeRegistry["resource"]>
  const done = deferred<void>(), admitted = deferred<void>(), write = deferred<void>()
  let nativeDone = false
  const { runtime, calls } = fixture({
    setup: () => { resource = native.resource({ initialized, state: () => ({ phase: "idle", reusable: true }), stop: async () => {} }) },
    turn: turn => {
      const scope = native.register(turn, resource, { done: done.promise, state: () => ({ idle: nativeDone, reusable: true }), stop: async () => {} })
      void write.promise.then(() => scope.run(async () => {
        await turn.output.write("native output after handler return")
        nativeDone = true
        done.resolve()
      })).catch(done.reject)
      admitted.resolve()
      return "returned"
    },
  }, { convergeNative: async (id, disposition) => { await native.join(id, disposition) } }, { failureConvergenceMs: 100, native })
  const pending = runtime.dispatch(request())
  await admitted.promise
  await new Promise(resolve => setImmediate(resolve))
  assert.equal(calls.includes("finalize"), false)
  write.resolve()
  assert.equal((await pending).status, "completed")
  assert.ok(calls.indexOf("output") < calls.indexOf("finalize"))
})

test("lost native continuation still joins authored callbacks before interrupted settlement", async () => {
  const started = deferred<void>(), callbackDone = deferred<void>(), handlerDone = deferred<void>()
  const { runtime, calls } = fixture({ turn: async turn => {
    await turn.onMessage(async () => { started.resolve(); await callbackDone.promise })
    await handlerDone.promise
    throw new Error("application failure before cleanup loss")
  } }, { convergeNative: async () => { throw new NativeContinuationLost() } })
  const pending = runtime.dispatch(request())
  await new Promise(resolve => setImmediate(resolve))
  const callback = runtime.deliverMessage("turn-1", "message", [])
  await started.promise
  handlerDone.resolve()
  await new Promise(resolve => setImmediate(resolve))
  assert.equal(calls.includes("fail"), false)
  callbackDone.resolve()
  await callback
  const outcome = await pending
  assert.equal(outcome.status, "interrupted")
  assert.match(outcome.error?.message ?? "", /application failure/)
  assert.ok(calls.includes("hold:native_continuation_lost"))
  assert.equal(runtime.idle, false)
  assert.throws(() => runtime.resume(), /reconstruction/)
})
