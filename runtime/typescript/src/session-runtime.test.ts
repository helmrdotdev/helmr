import { create, fromBinary, toBinary } from "@bufbuild/protobuf"
import { programProto } from "@helmr/proto"
import {
  actor,
  MessageRejected,
  tokens,
  timers,
  sessions,
  task,
  workspaces,
  type JsonValue,
  type Turn,
} from "@helmr/sdk"
import assert from "node:assert/strict"
import { test } from "node:test"
import { PassThrough } from "node:stream"
import { runProgram } from "./program"

const ids = {
  session: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc11",
  run: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc12",
  turn: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc13",
  event: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc14",
  hold: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc15",
  message: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc16",
  deployment: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc17",
  workspace: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc18",
}
type Event = programProto.RunEvent["event"]
function wire(
  schema: Parameters<typeof create>[0],
  value: unknown,
): Uint8Array {
  const body = toBinary(schema, value as never),
    result = new Uint8Array(body.length + 4)
  new DataView(result.buffer).setUint32(0, body.length)
  result.set(body, 4)
  return result
}
function deferred<T = void>() {
  return Promise.withResolvers<T>()
}

function harness(
  definition: unknown,
  inputs: unknown[] = [],
  intercept?: (
    event: Event,
    reply: (data?: unknown, kind?: string) => void,
  ) => boolean | Promise<boolean>,
  stream?: PassThrough,
) {
  const queue: Uint8Array[] = []
  let waiting: ((value: IteratorResult<Uint8Array>) => void) | undefined
  let closed = false,
    sequence = 0,
    outputSequence = 0
  const events: Event[] = []
  const push = (value: Uint8Array) => {
    if (stream !== undefined) {
      stream.write(value)
      return
    }
    if (waiting) {
      const resolve = waiting
      waiting = undefined
      resolve({ value, done: false })
    } else queue.push(value)
  }
  const start = create(programProto.ProgramStartSchema, {
    entrypointDeclaredId: "worker",
    runId: ids.run,
    attemptNumber: 1,
    deploymentId: ids.deployment,
    deploymentVersion: "v1",
    workspaceId: ids.workspace,
    baseWorkspaceVersionId: ids.event,
    cause: { kind: { case: "actorStart", value: {} } },
    entrypoint: {
      case: "actor",
      value: {
        sessionId: ids.session,
        startInputSequence: 0n,
        inputHighWatermark: BigInt(inputs.length),
        runGeneration: 7n,
      },
    },
  })
  push(wire(programProto.ProgramStartSchema, start))
  push(
    wire(
      programProto.EntrypointReleaseSchema,
      create(programProto.EntrypointReleaseSchema, {
        runId: ids.run,
        attemptNumber: 1,
        entrypoint: {
          declaredId: "worker",
          kind: { case: "actor", value: {} },
        },
      }),
    ),
  )
  const input: AsyncIterable<Uint8Array> = {
    [Symbol.asyncIterator]() {
      return {
        next: () =>
          queue.length > 0
            ? Promise.resolve({ value: queue.shift()!, done: false as const })
            : closed
              ? Promise.resolve({ value: undefined, done: true as const })
              : new Promise((resolve) => {
                  waiting = resolve
                }),
        return: async () => {
          closed = true
          waiting?.({ value: undefined, done: true })
          waiting = undefined
          return { value: undefined, done: true as const }
        },
      }
    },
  }
  const outcome = runProgram(new URL("file:///opt/program/declarations.json"), {
    input: stream ?? input,
    importModule: async () => ({ definition }),
    readLocator: async () =>
      JSON.stringify({
        formatVersion: 0,
        runtimeContract: "helmr.runtime.v0",
        architecture: "x86_64",
        configResultDigest: `sha256:${"4".repeat(64)}`,
        queues: [],
        declarations: [
          {
            kind: "actor",
            declaredId: "worker",
            manifest: {},
            locator: {
              exportName: "definition",
              sourcePath: "main.ts",
              slot: "handler",
            },
          },
        ],
      }),
    write: async (frame) => {
      const event = fromBinary(
        programProto.RunEventSchema,
        frame.subarray(4),
      ).event
      events.push(event)
      if (
        event.case === undefined ||
        event.case === "entrypointReady" ||
        event.case === "actorOutcome" ||
        event.case === "resumeConsumed"
      )
        return
      const value = event.value as {
        correlationId: string
        runWaitId?: string
        resumeAttachId?: string
      }
      const reply = (data: unknown = {}, kind = "completed") =>
        push(
          wire(
            programProto.ResumeDecisionSchema,
            create(programProto.ResumeDecisionSchema, {
              correlationId: value.correlationId,
              runWaitId: value.runWaitId ?? "",
              resumeAttachId: value.resumeAttachId ?? "",
              kind,
              dataJson: JSON.stringify(data),
            }),
          ),
        )
      if (await intercept?.(event, reply)) return
      if (
        event.case === "runWaitRequested" &&
        event.value.kind === "actor_input"
      ) {
        if (sequence === inputs.length)
          reply({ reason_code: "session_closed" }, "failed")
        else {
          sequence++
          reply({
            value: inputs[sequence - 1],
            run_generation: 7,
            turn: {
              id: sequence === 1 ? ids.turn : ids.message,
              sequence,
              created_at: "2026-09-19T00:00:00Z",
              source: { type: "external" },
            },
          })
        }
      } else if (event.case === "turnMessageClaimRequested")
        reply({ delivery: null })
      else if (
        event.case === "turnOutputWriteRequested" ||
        event.case === "sessionOutputWriteRequested"
      ) {
        reply({
          id: ids.event,
          sequence: ++outputSequence,
          session_id: ids.session,
          turn_id:
            event.case === "turnOutputWriteRequested"
              ? event.value.execution!.turnId
              : null,
          run_id: ids.run,
          attempt_number: 1,
          run_generation: 7,
        })
      } else if (event.case === "turnSettleRequested")
        reply(
          { event_id: ids.event, workspace_version_id: ids.event },
          "committed",
        )
      else if (
        event.case === "turnReadyRequested" ||
        event.case === "turnSettlementBeginRequested" ||
        event.case === "turnMessageCompleteRequested"
      )
        reply()
      else throw new Error(`unscripted event ${event.case}`)
    },
  })
  void outcome.catch(() => {})
  return {
    outcome,
    events,
    stop(turnId: string | null = ids.turn, runGeneration = 7) {
      push(
        wire(
          programProto.ResumeDecisionSchema,
          create(programProto.ResumeDecisionSchema, {
            kind: "session_stop",
            dataJson: JSON.stringify({
              execution: {
                session_id: ids.session,
                run_id: ids.run,
                attempt_number: 1,
                run_generation: runGeneration,
              },
              turn_id: turnId,
              hold_id: ids.hold,
              reason: "interrupt_requested",
            }),
          }),
        ),
      )
    },
  }
}

test("one Actor Run explicitly settles multiple Turns with distinct absent/null results", async () => {
  const seen: unknown[] = []
  const run = harness(
    actor({
      id: "worker",
      async run(session) {
        const one = (await session.receive())!
        seen.push(one.input)
        await assert.rejects(session.receive(), /explicitly settled/)
        await one.output.pipe(["working", "provider finished"])
        await one.complete()
        const two = (await session.receive())!
        seen.push(two.input)
        await assert.rejects(one.output.write("late"), /not writable/)
        await two.complete(null)
        assert.equal(await session.receive(), null)
      },
    }),
    ["one", "two"],
  )
  await run.outcome
  assert.deepEqual(seen, ["one", "two"])
  const settled = run.events
    .filter((x) => x.case === "turnSettleRequested")
    .map((x) => x.value)
  assert.equal(settled.length, 2)
  assert.equal(settled[0]!.resultJson, undefined)
  assert.equal(settled[1]!.resultJson, "null")
  assert.equal(run.events.at(-1)?.case, "actorOutcome")
})

test("Actor return and output EOF leave an unresolved Turn unsettled", async () => {
  const run = harness(
    actor({
      id: "worker",
      async run(session) {
        const turn = (await session.receive())!
        await turn.output.pipe([1, 2])
      },
    }),
    ["work"],
  )
  await run.outcome
  assert.equal(
    run.events.some((x) => x.case === "turnSettleRequested"),
    false,
  )
  const last = run.events.at(-1)!
  assert.equal(last.case, "actorOutcome")
  if (last.case === "actorOutcome")
    assert.equal(last.value.outcome.case, "failed")
})


test("unawaited write and pipe are drained before the durable settlement barrier", async () => {
  const release = deferred(),
    emitted = deferred()
  const run = harness(
    actor({
      id: "worker",
      async run(session) {
        const turn = (await session.receive())!
        void turn.output.pipe(
          (async function* () {
            await release.promise
            yield "late stream"
          })(),
        )
        emitted.resolve()
        await turn.complete()
      },
    }),
    ["work"],
  )
  await emitted.promise
  await new Promise((resolve) => setImmediate(resolve))
  assert.equal(
    run.events.some((x) => x.case === "turnSettlementBeginRequested"),
    false,
  )
  release.resolve()
  await run.outcome
  assert.ok(
    run.events.findIndex((x) => x.case === "turnOutputWriteRequested") <
      run.events.findIndex((x) => x.case === "turnSettlementBeginRequested"),
  )
})

test("an ignored output transport failure blocks terminal success", async () => {
  const run = harness(
    actor({
      id: "worker",
      async run(session) {
        const turn = (await session.receive())!
        void turn.output.write("uncertain")
        await turn.complete()
      },
    }),
    ["work"],
    (event) => {
      if (event.case === "turnOutputWriteRequested")
        throw new Error("transport lost")
      return false
    },
  )
  await assert.rejects(run.outcome, /runtime operation request/)
  assert.equal(
    run.events.some((x) => x.case === "turnSettleRequested"),
    false,
  )
})

test("readiness precedes output and callbacks are sequential with known rejection", async () => {
  const completed = deferred(),
    order: unknown[] = []
  let next = 0
  const run = harness(
    actor({
      id: "worker",
      async run(session) {
        const turn = (await session.receive())!
        void turn.onMessage(async ({ data: raw }) => {
          if (typeof raw !== "number") throw new MessageRejected("number required")
          const data = raw * 2
          order.push(data)
          await assert.rejects(turn.complete(), /callbacks cannot settle/)
          if (data === 2) throw new MessageRejected("already answered")
          await turn.output.write(data as JsonValue)
          completed.resolve()
        })
        await turn.output.write("request")
        await completed.promise
        await turn.complete()
      },
    }),
    ["work"],
    (event, reply) => {
      if (event.case === "turnMessageClaimRequested" && next < 3) {
        const data: unknown[] = ["invalid", 1, 2]
        reply({
          delivery: {
            message_id: ids.message,
            turn_id: ids.turn,
            delivery_id: event.value.deliveryId,
            sequence: ++next,
            data: data[next - 1],
          },
        })
        return true
      }
      return false
    },
  )
  await run.outcome
  assert.deepEqual(order, [2, 4])
  assert.ok(
    run.events.findIndex((x) => x.case === "turnReadyRequested") <
      run.events.findIndex((x) => x.case === "turnOutputWriteRequested"),
  )
  const dispositions = run.events
    .filter((x) => x.case === "turnMessageCompleteRequested")
    .map((x) => [x.value.status, x.value.code])
  assert.deepEqual(dispositions, [
    ["rejected", "handler_rejected"],
    ["rejected", "handler_rejected"],
    ["handled", ""],
  ])
  assert.ok(
    run.events.some(
      (x) =>
        x.case === "turnOutputWriteRequested" &&
        x.value.messageDeliveryId !== undefined,
    ),
  )
})

test("unknown callback failure is recorded and cannot become successful completion", async () => {
  const attempted = deferred()
  let delivered = false
  const run = harness(
    actor({
      id: "worker",
      async run(session, ctx) {
        const turn = (await session.receive())!
        await turn.onMessage(() => {
          throw new Error("native send uncertain")
        })
        await new Promise<void>((resolve) =>
          ctx.signal.addEventListener("abort", () => resolve(), { once: true }),
        )
        attempted.resolve()
        await turn.complete()
      },
    }),
    ["work"],
    (event, reply) => {
      if (event.case === "turnMessageClaimRequested" && !delivered) {
        delivered = true
        reply({
          delivery: {
            message_id: ids.message,
            turn_id: ids.turn,
            delivery_id: event.value.deliveryId,
            sequence: 1,
            data: {},
          },
        })
        return true
      }
      return false
    },
  )
  await attempted.promise
  await assert.rejects(run.outcome, /unknown/)
  assert.ok(
    run.events.some(
      (x) =>
        x.case === "turnMessageCompleteRequested" &&
        x.value.status === "unknown",
    ),
  )
  assert.equal(
    run.events.some((x) => x.case === "turnSettleRequested"),
    false,
  )
})

test("stop arrives independently while a message callback is pending", async () => {
  const callbackEntered = deferred(),
    release = deferred()
  let delivered = false,
    signal: AbortSignal | undefined
  const run = harness(
    actor({
      id: "worker",
      async run(session, ctx) {
        const turn = (await session.receive())!
        signal = turn.signal
        await turn.onMessage(async () => {
          callbackEntered.resolve()
          await release.promise
        })
        await new Promise<void>((resolve) =>
          ctx.signal.addEventListener("abort", () => resolve(), { once: true }),
        )
      },
    }),
    ["work"],
    (event, reply) => {
      if (event.case === "turnMessageClaimRequested" && !delivered) {
        delivered = true
        reply({
          delivery: {
            message_id: ids.message,
            turn_id: ids.turn,
            delivery_id: event.value.deliveryId,
            sequence: 1,
            data: {},
          },
        })
        return true
      }
      return false
    },
  )
  await callbackEntered.promise
  run.stop()
  await new Promise((resolve) => setImmediate(resolve))
  assert.equal(signal?.aborted, true)
  assert.equal(
    run.events.some((x) => x.case === "actorOutcome"),
    false,
  )
  release.resolve()
  await run.outcome
  const outcome = run.events.at(-1)!
  if (outcome.case !== "actorOutcome") assert.fail("missing outcome")
  assert.equal(outcome.value.outcome.case, "interrupted")
})

test("managed waits retain exact Turn scope and the single consuming gate", async () => {
  let current: Turn | undefined
  const run = harness(
    actor({
      id: "worker",
      async run(session) {
        current = (await session.receive())!
        const wait = tokens.ref(ids.event).wait().unwrap()
        await assert.rejects(timers.waitFor("1s"), /one consuming Wait/)
        assert.deepEqual(await wait, { answer: true })
        await current.complete()
      },
    }),
    ["work"],
    (event, reply) => {
      if (event.case === "runWaitRequested" && event.value.kind === "token") {
        assert.equal(event.value.turnId, current!.id)
        assert.equal(event.value.execution?.runGeneration, 7n)
        setImmediate(() => reply({ answer: true }))
        return true
      }
      return false
    },
  )
  await run.outcome
})

test("stop aborts an active Token wait without exposing its result or consuming it", async () => {
  const waiting = deferred()
  let cancel: (() => void) | undefined
  let exposed = false
  const run = harness(
    actor({
      id: "worker",
      async run(session, ctx) {
        const turn = (await session.receive())!
        try {
          await tokens.ref(ids.event).wait().unwrap()
          exposed = true
        } catch (error) {
          assert.equal(error, ctx.signal.reason)
          assert.equal(turn.signal.aborted, true)
        }
      },
    }),
    ["work"],
    (event, reply) => {
      if (event.case === "runWaitRequested" && event.value.kind === "token") {
        cancel = () => reply({ reason_code: "session_stopped" }, "cancelled")
        waiting.resolve()
        return true
      }
      return false
    },
  )
  await waiting.promise
  run.stop()
  cancel!()
  await run.outcome
  assert.equal(exposed, false)
  assert.equal(
    run.events.some((x) => x.case === "resumeConsumed"),
    false,
  )
  const last = run.events.at(-1)!
  if (last.case !== "actorOutcome") assert.fail("missing outcome")
  assert.equal(last.value.outcome.case, "interrupted")
})

test("a cancelled shared Token does not interrupt its Actor", async () => {
  const run = harness(
    actor({
      id: "worker",
      async run(session, ctx) {
        const turn = (await session.receive())!
        const result = await tokens.ref(ids.event).wait()
        assert.equal(result.ok, false)
        assert.equal(ctx.signal.aborted, false)
        assert.equal(turn.signal.aborted, false)
        await turn.complete()
      },
    }),
    ["work"],
    (event, reply) => {
      if (event.case === "runWaitRequested" && event.value.kind === "token") {
        reply({ reason_code: "token_cancelled" }, "cancelled")
        return true
      }
      return false
    },
  )
  await run.outcome
  assert.ok(run.events.some((x) => x.case === "turnSettleRequested"))
})

test("stop wins while settlement barrier is pending", async () => {
  const requested = deferred()
  let rejectBarrier: (() => void) | undefined
  const run = harness(
    actor({
      id: "worker",
      async run(session) {
        await (await session.receive())!.complete()
      },
    }),
    ["work"],
    (event, reply) => {
      if (event.case === "turnSettlementBeginRequested") {
        rejectBarrier = () =>
          reply(
            { code: "turn_stopping", message: "stopping", retryable: false },
            "failed",
          )
        requested.resolve()
        return true
      }
      return false
    },
  )
  await requested.promise
  run.stop()
  rejectBarrier!()
  await run.outcome
  assert.equal(
    run.events.some((x) => x.case === "turnSettleRequested"),
    false,
  )
  const last = run.events.at(-1)!
  if (last.case !== "actorOutcome") assert.fail("missing outcome")
  assert.equal(last.value.outcome.case, "interrupted")
})

test("a stopped output rejects permission before the local stop signal arrives", async () => {
  const denied = deferred()
  let allow = false
  const run = harness(
    actor({
      id: "worker",
      async run(session, ctx) {
        const turn = (await session.receive())!
        try {
          await turn.output.write({ type: "permission_admitted" })
          allow = true
        } catch (error) {
          assert.equal((error as { code: string }).code, "turn_stopping")
          assert.equal(ctx.signal.aborted, false)
          denied.resolve()
        }
        await new Promise<void>((resolve) =>
          ctx.signal.addEventListener("abort", () => resolve(), { once: true }),
        )
      },
    }),
    ["work"],
    (event, reply) => {
      if (event.case === "turnOutputWriteRequested") {
        reply(
          { code: "turn_stopping", message: "stopping", retryable: false },
          "failed",
        )
        return true
      }
      return false
    },
  )
  await denied.promise
  assert.equal(allow, false)
  run.stop()
  await run.outcome
})

test("stop waits for an admitted output response instead of abandoning its correlation", async () => {
  const requested = deferred()
  let finish: (() => void) | undefined
  const run = harness(
    actor({
      id: "worker",
      async run(session) {
        const turn = (await session.receive())!
        await turn.output.write({ type: "permission_admitted" })
        assert.equal(turn.signal.aborted, true)
      },
    }),
    ["work"],
    (event, reply) => {
      if (event.case === "turnOutputWriteRequested") {
        finish = () =>
          reply({
            id: ids.event,
            sequence: 1,
            session_id: ids.session,
            turn_id: ids.turn,
            run_id: ids.run,
            attempt_number: 1,
            run_generation: 7,
          })
        requested.resolve()
        return true
      }
      return false
    },
  )
  await requested.promise
  run.stop()
  await new Promise((resolve) => setImmediate(resolve))
  assert.equal(
    run.events.some((x) => x.case === "actorOutcome"),
    false,
  )
  finish!()
  await run.outcome
})

test("initialization stop carries null Turn and does not invent a terminal Turn", async () => {
  const entered = deferred()
  const run = harness(
    actor({
      id: "worker",
      async run(_session, ctx) {
        entered.resolve()
        await new Promise<void>((resolve) =>
          ctx.signal.addEventListener("abort", () => resolve(), { once: true }),
        )
      },
    }),
  )
  await entered.promise
  run.stop(null)
  await run.outcome
  const last = run.events.at(-1)!
  if (last.case !== "actorOutcome" || last.value.outcome.case !== "interrupted")
    assert.fail("missing interrupted outcome")
  assert.equal(last.value.outcome.value.turnId, undefined)
})

test("wrong-generation delivery stays a protocol fault even when application catches it", async () => {
  const run = harness(
    actor({
      id: "worker",
      async run(session) {
        await assert.rejects(session.receive(), /frontier/)
      },
    }),
    ["work"],
    (event, reply) => {
      if (event.case === "runWaitRequested") {
        reply({
          value: "work",
          run_generation: 8,
          turn: {
            id: ids.turn,
            sequence: 1,
            created_at: "2026-09-19T00:00:00Z",
            source: { type: "external" },
          },
        })
        return true
      }
      return false
    },
  )
  await assert.rejects(run.outcome, /frontier/)
  assert.equal(
    run.events.some((x) => x.case === "actorOutcome"),
    false,
  )
})

test("a historical output receipt cannot be adopted by another producer", async () => {
  const run = harness(
    actor({
      id: "worker",
      async run(session) {
        const turn = (await session.receive())!
        void turn.output.write("new")
        await turn.complete()
      },
    }),
    ["work"],
    (event, reply) => {
      if (event.case === "turnOutputWriteRequested") {
        reply({
          id: ids.event,
          sequence: 1,
          session_id: ids.session,
          turn_id: ids.turn,
          run_id: ids.run,
          attempt_number: 1,
          run_generation: 6,
        })
        return true
      }
      return false
    },
  )
  await assert.rejects(run.outcome, /producer/)
  assert.equal(
    run.events.some((x) => x.case === "turnSettleRequested"),
    false,
  )
})

test("retained message handler redeclares readiness after a managed wait", async () => {
  let readiness = 0
  const run = harness(
    actor({
      id: "worker",
      async run(session) {
        const turn = (await session.receive())!
        await turn.onMessage(() => {})
        await tokens.ref(ids.event).wait().unwrap()
        assert.equal(readiness, 2)
        await turn.complete()
      },
    }),
    ["work"],
    (event, reply) => {
      if (event.case === "turnReadyRequested") {
        readiness++
        reply()
        return true
      }
      if (event.case === "runWaitRequested" && event.value.kind === "token") {
        reply(true)
        return true
      }
      return false
    },
  )
  await run.outcome
})

test("Actor child calls carry exact Turn identity while Task results stay generic", async () => {
  const child = task({ id: "child", run: () => null })
  const run = harness(
    actor({
      id: "worker",
      async run(session) {
        const turn = (await session.receive())!
        const result = await child.call({
          workspace: workspaces.ref(ids.workspace),
          idempotencyKey: "owned-child",
        })
        assert.equal(result.ok, true)
        await turn.complete()
      },
    }),
    ["work"],
    (event, reply) => {
      if (event.case === "taskChildInvokeRequested") {
        assert.equal(event.value.turnId, ids.turn)
        assert.equal(event.value.execution?.sessionId, ids.session)
        assert.equal(event.value.execution?.runGeneration, 7n)
        assert.equal(event.value.actorSpeculativeInputSequence, 1n)
        reply({ ok: true, output: null, run: { id: ids.event } })
        return true
      }
      return false
    },
  )
  await run.outcome
})

test("runtime Session addresses use new admission, exact message and control commands", async () => {
  const modes: string[] = []
  const run = harness(
    actor({
      id: "worker",
      async run() {
        const ref = sessions.ref(ids.session)
        const enqueued = await ref.enqueue("ordinary")
        assert.equal(enqueued.id, ids.turn)
        const submitted = await ref.send("update")
        assert.equal(submitted.kind, "messaged")
        const message = await ref.turn(ids.turn).send("exact")
        assert.equal(message.id, ids.message)
        const stopped = await ref.turn(ids.turn).interrupt()
        assert.equal(stopped.holdId, ids.hold)
        await ref.close()
        await ref.resume({ holdId: ids.hold })
        const page = await ref.events.list()
        assert.equal(page.retainedAfter, 0)
      },
    }),
    [],
    (event, reply) => {
      if (event.case === "sessionSubmitRequested") {
        modes.push(event.value.mode)
        assert.notEqual(event.value.idempotencyKey, undefined)
        if (event.value.mode === "enqueue")
          reply({ id: ids.event, kind: "enqueued", turn_id: ids.turn })
        else if (event.value.mode === "send")
          reply({
            id: ids.event,
            kind: "messaged",
            turn_id: ids.turn,
            message_id: ids.message,
          })
        else {
          assert.equal(event.value.turnId, ids.turn)
          reply({
            id: ids.event,
            turn_id: ids.turn,
            message_id: ids.message,
            status: "accepted",
          })
        }
        return true
      }
      if (event.case === "sessionTurnInterruptRequested") {
        reply({
          id: ids.event,
          session_id: ids.session,
          turn_id: ids.turn,
          hold_id: ids.hold,
          status: "accepted",
        })
        return true
      }
      if (event.case === "sessionCloseRequested") {
        reply({ id: ids.event, session_id: ids.session, status: "accepted" })
        return true
      }
      if (event.case === "sessionResumeRequested") {
        assert.equal(event.value.holdId, ids.hold)
        reply({
          id: ids.event,
          session_id: ids.session,
          hold_id: ids.hold,
          status: "accepted",
        })
        return true
      }
      if (event.case === "sessionEventsRequested") {
        reply({
          records: [],
          next_after: 0,
          has_more: false,
          retained_after: 0,
        })
        return true
      }
      return false
    },
  )
  await run.outcome
  assert.deepEqual(modes, ["enqueue", "send", "message"])
})

test("a plain unawaited write receipt drains before settlement begins", async () => {
  const requested = deferred()
  let receipt: (() => void) | undefined
  const run = harness(
    actor({
      id: "worker",
      async run(session) {
        const turn = (await session.receive())!
        void turn.output.write("unawaited")
        await turn.complete()
      },
    }),
    ["work"],
    (event, reply) => {
      if (event.case !== "turnOutputWriteRequested") return false
      receipt = () =>
        reply({
          id: ids.event,
          sequence: 1,
          session_id: ids.session,
          turn_id: ids.turn,
          run_id: ids.run,
          attempt_number: 1,
          run_generation: 7,
        })
      requested.resolve()
      return true
    },
  )
  await requested.promise
  await new Promise((resolve) => setImmediate(resolve))
  assert.equal(
    run.events.some((event) => event.case === "turnSettlementBeginRequested"),
    false,
  )
  receipt!()
  await run.outcome
  assert.equal(
    run.events.filter((event) => event.case === "turnSettleRequested").length,
    1,
  )
})

test("stop binds the activated Turn while its receive response is still pending", async () => {
  const waiting = deferred()
  let cancelled: (() => void) | undefined
  let exposed = false
  const run = harness(
    actor({
      id: "worker",
      async run(session, ctx) {
        try {
          await session.receive()
          exposed = true
        } catch (error) {
          assert.equal(error, ctx.signal.reason)
        }
      },
    }),
    ["work"],
    (event, reply) => {
      if (event.case !== "runWaitRequested") return false
      cancelled = () => reply({ reason_code: "session_stopped" }, "cancelled")
      waiting.resolve()
      return true
    },
  )
  await waiting.promise
  run.stop()
  cancelled!()
  await run.outcome
  assert.equal(exposed, false)
  assert.equal(
    run.events.some(
      (event) =>
        event.case === "turnSettleRequested" || event.case === "resumeConsumed",
    ),
    false,
  )
  const event = run.events.at(-1)!
  if (
    event.case !== "actorOutcome" ||
    event.value.outcome.case !== "interrupted"
  )
    assert.fail("missing interrupted outcome")
  assert.equal(event.value.outcome.value.turnId, ids.turn)
})

test("a stop naming another Turn is rejected once the heap owns a Turn", async () => {
  const active = deferred()
  const run = harness(
    actor({
      id: "worker",
      async run(session, ctx) {
        await session.receive()
        active.resolve()
        await new Promise<void>((resolve) =>
          ctx.signal.addEventListener("abort", () => resolve(), { once: true }),
        )
      },
    }),
    ["work"],
  )
  await active.promise
  run.stop(ids.message)
  await assert.rejects(run.outcome, /control transport/)
  assert.equal(
    run.events.some((event) => event.case === "actorOutcome"),
    false,
  )
})

test("an Actor throwing undefined is still a failed Run", async () => {
  const run = harness(
    actor({
      id: "worker",
      run() {
        throw undefined
      },
    }),
  )
  await run.outcome
  const event = run.events.at(-1)!
  if (event.case !== "actorOutcome") assert.fail("missing Actor outcome")
  assert.equal(event.value.outcome.case, "failed")
})

test("pending receive does not admit a stop from a stale execution generation", async () => {
  const waiting = deferred()
  const run = harness(
    actor({
      id: "worker",
      async run(session) {
        await session.receive()
      },
    }),
    ["work"],
    (event) => {
      if (event.case !== "runWaitRequested") return false
      waiting.resolve()
      return true
    },
  )
  await waiting.promise
  run.stop(ids.turn, 6)
  await assert.rejects(run.outcome, /control transport|operation decision/)
  assert.equal(
    run.events.some((event) => event.case === "actorOutcome"),
    false,
  )
})

test("outside-Turn managed waits carry generation and the committed input position", async () => {
  const positions: bigint[] = []
  const run = harness(
    actor({
      id: "worker",
      async run(session) {
        await tokens.ref(ids.event).wait().unwrap()
        const turn = (await session.receive())!
        await turn.complete()
        await tokens.ref(ids.event).wait().unwrap()
      },
    }),
    ["work"],
    (event, reply) => {
      if (event.case !== "runWaitRequested" || event.value.kind !== "token")
        return false
      assert.equal(event.value.execution?.runGeneration, 7n)
      assert.equal(event.value.turnId, undefined)
      positions.push(event.value.actorSpeculativeInputSequence!)
      reply(true)
      return true
    },
  )
  await run.outcome
  assert.deepEqual(positions, [0n, 1n])
})

test("Actor completion closes a real Node input stream with an idle control read", async () => {
  const stream = new PassThrough()
  const run = harness(actor({ id: "worker", run() {} }), [], undefined, stream)
  await run.outcome
  assert.equal(stream.destroyed, true)
  const event = run.events.at(-1)!
  if (event.case !== "actorOutcome") assert.fail("missing Actor outcome")
  assert.equal(event.value.outcome.case, "succeeded")
})

test("null-Turn stop waits for a pending committed settlement response", async () => {
  const requested = deferred()
  let committed: (() => void) | undefined
  let settled = false
  let signal: AbortSignal | undefined
  const run = harness(
    actor({
      id: "worker",
      async run(session, ctx) {
        const turn = (await session.receive())!
        signal = ctx.signal
        await turn.complete("done")
        settled = true
        await assert.rejects(
          session.receive(),
          (error) => error === ctx.signal.reason,
        )
      },
    }),
    ["work"],
    (event, reply) => {
      if (event.case !== "turnSettleRequested") return false
      committed = () =>
        reply(
          { event_id: ids.event, workspace_version_id: ids.event },
          "committed",
        )
      requested.resolve()
      return true
    },
  )
  await requested.promise
  run.stop(null)
  await new Promise((resolve) => setImmediate(resolve))
  assert.equal(signal?.aborted, true)
  assert.equal(settled, false)
  assert.equal(
    run.events.some((event) => event.case === "actorOutcome"),
    false,
  )
  committed!()
  await run.outcome
  assert.equal(settled, true)
  const event = run.events.at(-1)!
  if (
    event.case !== "actorOutcome" ||
    event.value.outcome.case !== "interrupted"
  )
    assert.fail("missing interrupted outcome")
  assert.equal(event.value.outcome.value.turnId, undefined)
  assert.equal(event.value.outcome.value.holdId, ids.hold)
})

test("null-Turn stop cannot bypass an active Turn's settlement preparation", async () => {
  const barrier = deferred()
  const run = harness(
    actor({
      id: "worker",
      async run(session) {
        await (await session.receive())!.complete()
      },
    }),
    ["work"],
    (event) => {
      if (event.case !== "turnSettlementBeginRequested") return false
      barrier.resolve()
      return true
    },
  )
  await barrier.promise
  run.stop(null)
  await assert.rejects(run.outcome, /control transport|operation decision/)
  assert.equal(
    run.events.some(
      (event) =>
        event.case === "turnSettleRequested" || event.case === "actorOutcome",
    ),
    false,
  )
})

test("null-Turn stop with a rejected pending settlement remains uncertain", async () => {
  const requested = deferred()
  let rejected: (() => void) | undefined
  const run = harness(
    actor({
      id: "worker",
      async run(session) {
        await (await session.receive())!.complete()
      },
    }),
    ["work"],
    (event, reply) => {
      if (event.case !== "turnSettleRequested") return false
      rejected = () =>
        reply(
          { code: "turn_stopping", message: "stopping", retryable: false },
          "failed",
        )
      requested.resolve()
      return true
    },
  )
  await requested.promise
  run.stop(null)
  rejected!()
  await assert.rejects(run.outcome, /Null-Turn stop was not confirmed/)
  assert.equal(
    run.events.some((event) => event.case === "actorOutcome"),
    false,
  )
})

test("a committed settlement cannot contradict an exact active-Turn stop", async () => {
  const requested = deferred()
  let committed: (() => void) | undefined
  const run = harness(
    actor({
      id: "worker",
      async run(session) {
        await (await session.receive())!.complete()
      },
    }),
    ["work"],
    (event, reply) => {
      if (event.case !== "turnSettleRequested") return false
      committed = () =>
        reply(
          { event_id: ids.event, workspace_version_id: ids.event },
          "committed",
        )
      requested.resolve()
      return true
    },
  )
  await requested.promise
  run.stop()
  committed!()
  await assert.rejects(run.outcome, /committed after a Turn stop/)
  assert.equal(
    run.events.some((event) => event.case === "actorOutcome"),
    false,
  )
})
