import { create, fromBinary, toBinary, type GenMessage } from "@bufbuild/protobuf"
import { runProto } from "@helmr/proto"
import {
  actor,
  image,
  logger,
  metadata,
  task,
  timers,
  tokens,
  workspace,
  workspaces,
} from "@helmr/sdk"
import { describe, expect, test } from "bun:test"

import { runProgram, type ProgramIO } from "./program"

const locatorURL = new URL(
  "file:///opt/helmr/program/helmr/declarations.json",
)

describe("runProgram", () => {
  test("reports an Actor return with its terminal cursor", async () => {
    let actorID = ""
    const definition = actor({
      id: "worker",
      run(_self, ctx) { actorID = ctx.actor.id },
    })
    const start = actorStart(0n, 0n)
    const output: Uint8Array[] = []
    await runProgram(locatorURL, programIO({
      input: frames(
        frameMessage(runProto.ProgramStartSchema, start),
        frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start)),
      ),
      definition,
      output,
    }))
    expect(actorID).toBe("actor-1")
    const outcome = readEvent(output[1]!).event
    expect(outcome.case).toBe("actorOutcome")
    if (outcome.case === "actorOutcome") {
      expect(outcome.value.terminalInputSequence).toBe(0n)
      expect(outcome.value.outcome.case).toBe("succeeded")
    }
  })

  test("reports an Actor throw as a bounded failure with its cursor", async () => {
    const definition = actor({ id: "worker", run() { throw new Error("boom") } })
    const start = actorStart(4n, 7n)
    const output: Uint8Array[] = []
    await runProgram(locatorURL, programIO({
      input: frames(
        frameMessage(runProto.ProgramStartSchema, start),
        frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start)),
      ),
      definition,
      output,
    }))
    const outcome = readEvent(output[1]!).event
    expect(outcome.case).toBe("actorOutcome")
    if (outcome.case === "actorOutcome") {
      expect(outcome.value.terminalInputSequence).toBe(4n)
      expect(outcome.value.outcome.case).toBe("failed")
    }
  })

  test("commits the prior Actor turn before delivering the next input", async () => {
    const received: unknown[] = []
    const definition = actor({
      id: "worker",
      async run(self) {
        received.push(await self.input.receive({ timeout: "1ms", idleTimeout: "1501ms" }).unwrap())
        received.push(await self.input.receive().unwrap())
      },
    })
    const start = actorStart(0n, 2n)
    const output: Uint8Array[] = []
    const events = [deferred<void>(), deferred<void>(), deferred<void>()]
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(runProto.ProgramStartSchema, start)
      yield frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start))

      await events[0]!.promise
      const first = readEvent(output[1]!).event
      expect(first.case).toBe("runWaitRequested")
      if (first.case !== "runWaitRequested") return
      expect(first.value.kind).toBe("actor_input")
      expect(JSON.parse(first.value.paramsJson).after_input_sequence).toBe(0)
      expect(first.value.actorSpeculativeInputSequence).toBe(0n)
      expect(first.value.timeoutMs).toBe(1n)
      expect(first.value.idleTimeoutMs).toBe(1501n)
      yield actorDecision(first.value.correlationId, "completed", actorInput(1, "one"))

      await events[1]!.promise
      const commit = readEvent(output[2]!).event
      expect(commit.case).toBe("actorTurnCommitRequested")
      if (commit.case !== "actorTurnCommitRequested") return
      expect(commit.value.targetInputSequence).toBe(1n)
      yield actorDecision(commit.value.correlationId, "committed", "null")

      await events[2]!.promise
      const second = readEvent(output[3]!).event
      expect(second.case).toBe("runWaitRequested")
      if (second.case !== "runWaitRequested") return
      expect(JSON.parse(second.value.paramsJson).after_input_sequence).toBe(1)
      expect(second.value.actorSpeculativeInputSequence).toBe(1n)
      yield actorDecision(second.value.correlationId, "completed", actorInput(2, "two"))
    }
    await runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => {
        if (output.length >= 2 && output.length <= 4) {
          events[output.length - 2]!.resolve()
        }
      },
    }))
    expect(received).toEqual(["one", "two"])
    const outcome = readEvent(output[4]!).event
    expect(outcome.case).toBe("actorOutcome")
    if (outcome.case === "actorOutcome") {
      expect(outcome.value.terminalInputSequence).toBe(2n)
      expect(outcome.value.outcome.case).toBe("succeeded")
    }
  })

  test("rejects an overlapping Actor receive before emitting a second Wait", async () => {
    let overlapError = ""
    const definition = actor({
      id: "worker",
      async run(self) {
        const first = self.input.receive()
        try {
          await self.input.receive()
        } catch (error) {
          overlapError = error instanceof Error ? error.name : String(error)
        }
        await first
      },
    })
    const start = actorStart(0n, 1n)
    const output: Uint8Array[] = []
    const waitWritten = deferred<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(runProto.ProgramStartSchema, start)
      yield frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start))
      await waitWritten.promise
      const wait = readEvent(output[1]!).event
      if (wait.case !== "runWaitRequested") return
      yield actorDecision(wait.value.correlationId, "completed", actorInput(1, "one"))
    }
    await runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => { if (output.length === 2) waitWritten.resolve() },
    }))
    expect(overlapError).toBe("ConcurrentActorReceiveError")
    expect(output.map((frame) => readEvent(frame).event.case)).toEqual([
      "entrypointReady",
      "runWaitRequested",
      "actorOutcome",
    ])
  })

  test("shares the consuming Wait guard between Actor input and timers", async () => {
    let timerError = ""
    const definition = actor({
      id: "worker",
      async run(self) {
        const receive = self.input.receive()
        try {
          await timers.waitFor("1m")
        } catch (error) {
          timerError = error instanceof Error ? error.message : String(error)
        }
        await receive
      },
    })
    const start = actorStart(0n, 1n)
    const output: Uint8Array[] = []
    const waitWritten = deferred<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(runProto.ProgramStartSchema, start)
      yield frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start))
      await waitWritten.promise
      const wait = readEvent(output[1]!).event
      if (wait.case !== "runWaitRequested") return
      yield actorDecision(wait.value.correlationId, "completed", actorInput(1, "one"))
    }
    await runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => { if (output.length === 2) waitWritten.resolve() },
    }))
    expect(timerError).toBe("only one consuming Wait may be pending")
    expect(output.map((frame) => readEvent(frame).event.case)).toEqual([
      "entrypointReady",
      "runWaitRequested",
      "actorOutcome",
    ])
  })

  test("aborts Actor context when a timer Wait is cancelled", async () => {
    let signalAborted = false
    const definition = actor({
      id: "worker",
      async run(_self, ctx) {
        try {
          await timers.waitFor("1m")
        } catch {
          signalAborted = ctx.signal.aborted
        }
      },
    })
    const start = actorStart(0n, 0n)
    const output: Uint8Array[] = []
    const waitWritten = deferred<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(runProto.ProgramStartSchema, start)
      yield frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start))
      await waitWritten.promise
      const wait = readEvent(output[1]!).event
      if (wait.case !== "runWaitRequested") return
      yield actorDecision(
        wait.value.correlationId,
        "cancelled",
        JSON.stringify({ reason_code: "run_cancelled" }),
      )
    }
    await expect(runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => { if (output.length === 2) waitWritten.resolve() },
    }))).rejects.toThrow("run_cancelled")
    expect(signalAborted).toBe(true)
    expect(output).toHaveLength(2)
  })

  test("treats a non-contiguous Actor input record as a runtime protocol fault", async () => {
    const definition = actor({
      id: "worker",
      async run(self) { await self.input.receive() },
    })
    const start = actorStart(0n, 2n)
    const output: Uint8Array[] = []
    const waitWritten = deferred<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(runProto.ProgramStartSchema, start)
      yield frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start))
      await waitWritten.promise
      const wait = readEvent(output[1]!).event
      if (wait.case !== "runWaitRequested") return
      yield actorDecision(wait.value.correlationId, "completed", actorInput(2, "skipped"))
    }
    await expect(runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => { if (output.length === 2) waitWritten.resolve() },
    }))).rejects.toThrow("next contiguous record")
    expect(output.map((frame) => readEvent(frame).event.case)).toEqual([
      "entrypointReady",
      "runWaitRequested",
    ])
  })

  test("latches an Actor protocol fault even when user code catches it", async () => {
    let caught = false
    const definition = actor({
      id: "worker",
      async run(self) {
        try {
          await self.input.receive()
        } catch {
          caught = true
        }
      },
    })
    const start = actorStart(0n, 2n)
    const output: Uint8Array[] = []
    const waitWritten = deferred<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(runProto.ProgramStartSchema, start)
      yield frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start))
      await waitWritten.promise
      const wait = readEvent(output[1]!).event
      if (wait.case !== "runWaitRequested") return
      yield actorDecision(wait.value.correlationId, "completed", actorInput(2, "skipped"))
    }
    await expect(runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => { if (output.length === 2) waitWritten.resolve() },
    }))).rejects.toThrow("next contiguous record")
    expect(caught).toBe(true)
    expect(output).toHaveLength(2)
  })

  test("aborts Actor context when an input Wait is cancelled", async () => {
    let caughtName = ""
    let signalAborted = false
    let signalReason = ""
    const definition = actor({
      id: "worker",
      async run(self, ctx) {
        try {
          await self.input.receive()
        } catch (error) {
          caughtName = error instanceof Error ? error.name : String(error)
          signalAborted = ctx.signal.aborted
          signalReason = ctx.signal.reason instanceof Error
            ? ctx.signal.reason.message
            : String(ctx.signal.reason)
        }
      },
    })
    const start = actorStart(0n, 1n)
    const output: Uint8Array[] = []
    const waitWritten = deferred<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(runProto.ProgramStartSchema, start)
      yield frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start))
      await waitWritten.promise
      const wait = readEvent(output[1]!).event
      if (wait.case !== "runWaitRequested") return
      yield actorDecision(
        wait.value.correlationId,
        "cancelled",
        JSON.stringify({ reason_code: "run_cancelled" }),
      )
    }
    await expect(runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => { if (output.length === 2) waitWritten.resolve() },
    }))).rejects.toThrow("run_cancelled")
    expect(caughtName).toBe("AbortError")
    expect(signalAborted).toBe(true)
    expect(signalReason).toContain("run_cancelled")
    expect(output).toHaveLength(2)
  })

  test("rejects malformed Actor channel records as runtime protocol faults", async () => {
    const malformed = [
      JSON.stringify({
        record: {
          id: "record-1",
          sequence: 1,
          created_at: "2026-07-22T00:00:00Z",
          source: { type: "external" },
        },
      }),
      JSON.stringify({
        id: "output-1",
        sequence: 1,
        content_type: "application/json",
        created_at: "2026-07-22T00:00:00Z",
        provenance: {
          run_id: "run-1",
          attempt_number: 1,
          deployment_id: "deployment-1",
        },
      }),
    ]
    for (const [index, dataJson] of malformed.entries()) {
      const definition = actor({
        id: "worker",
        async run(self) {
          if (index === 0) await self.input.receive()
          else await self.output.append("value")
        },
      })
      const start = actorStart(0n, 1n)
      const output: Uint8Array[] = []
      const requestWritten = deferred<void>()
      async function* input(): AsyncIterable<Uint8Array> {
        yield frameMessage(runProto.ProgramStartSchema, start)
        yield frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start))
        await requestWritten.promise
        const request = readEvent(output[1]!).event
        const correlationId = request.case === "runWaitRequested"
          ? request.value.correlationId
          : request.case === "actorOutputAppendRequested"
          ? request.value.correlationId
          : ""
        yield actorDecision(correlationId, "completed", dataJson)
      }
      await expect(runProgram(locatorURL, programIO({
        input: input(),
        definition,
        output,
        onWrite: () => { if (output.length === 2) requestWritten.resolve() },
      }))).rejects.toThrow("was invalid")
      expect(output).toHaveLength(2)
    }
  })

  test("does not translate an Actor channel write failure into an Actor outcome", async () => {
    const definition = actor({
      id: "worker",
      async run(self) { await self.input.receive() },
    })
    const start = actorStart(0n, 1n)
    const output: Uint8Array[] = []
    await expect(runProgram(locatorURL, programIO({
      input: frames(
        frameMessage(runProto.ProgramStartSchema, start),
        frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start)),
      ),
      definition,
      output,
      failWriteAt: 2,
    }))).rejects.toThrow("failed to write runtime operation request")
    expect(output.map((frame) => readEvent(frame).event.case)).toEqual([
      "entrypointReady",
    ])
  })

  test("rejects Actor success while a channel operation is still pending", async () => {
    const definition = actor({
      id: "worker",
      run(self) { void self.output.append("unawaited") },
    })
    const start = actorStart(0n, 0n)
    const output: Uint8Array[] = []
    await expect(runProgram(locatorURL, programIO({
      input: frames(
        frameMessage(runProto.ProgramStartSchema, start),
        frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start)),
      ),
      definition,
      output,
    }))).rejects.toThrow("runtime operations still pending")
    expect(output.map((frame) => readEvent(frame).event.case)).toEqual([
      "entrypointReady",
      "actorOutputAppendRequested",
    ])
  })

  test("tracks output pipe before it emits its first append", async () => {
    const sourceGate = deferred<void>()
    const definition = actor({
      id: "worker",
      run(self) {
        void self.output.pipe((async function* () {
          await sourceGate.promise
          yield "late"
        })())
      },
    })
    const start = actorStart(0n, 0n)
    const output: Uint8Array[] = []
    await expect(runProgram(locatorURL, programIO({
      input: frames(
        frameMessage(runProto.ProgramStartSchema, start),
        frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start)),
      ),
      definition,
      output,
    }))).rejects.toThrow("runtime operations still pending")
    expect(output.map((frame) => readEvent(frame).event.case)).toEqual([
      "entrypointReady",
    ])
  })

  test("appends Actor output through the runtime-bound producer channel", async () => {
    let appended: unknown
    const definition = actor({
      id: "worker",
      async run(self) {
        appended = await self.output.append(
          { event: "ready" },
          { contentType: "application/vnd.helmr.test+json", idempotencyKey: "output-1" },
        )
      },
    })
    const start = actorStart(0n, 0n)
    const output: Uint8Array[] = []
    const appendWritten = deferred<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(runProto.ProgramStartSchema, start)
      yield frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start))
      await appendWritten.promise
      const event = readEvent(output[1]!).event
      expect(event.case).toBe("actorOutputAppendRequested")
      if (event.case !== "actorOutputAppendRequested") return
      expect(JSON.parse(event.value.dataJson)).toEqual({ event: "ready" })
      expect(event.value.contentType).toBe("application/vnd.helmr.test+json")
      expect(event.value.idempotencyKey).toBe("output-1")
      yield actorDecision(event.value.correlationId, "completed", JSON.stringify({
        id: "output-1",
        sequence: 1,
        data: { event: "ready" },
        content_type: event.value.contentType,
        created_at: "2026-07-22T00:00:00Z",
        provenance: {
          run_id: "run-1",
          attempt_number: 1,
          deployment_id: "deployment-1",
        },
      }))
    }
    await runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => { if (output.length === 2) appendWritten.resolve() },
    }))
    expect(appended).toEqual({
      id: "output-1",
      sequence: 1,
      data: { event: "ready" },
      contentType: "application/vnd.helmr.test+json",
      createdAt: "2026-07-22T00:00:00Z",
      provenance: {
        runId: "run-1",
        attemptNumber: 1,
        deploymentId: "deployment-1",
      },
    })
  })

  test("exposes Actor output semantic failures to user code", async () => {
    let failure: unknown
    const definition = actor({
      id: "worker",
      async run(self) {
        try {
          await self.output.append("value", { idempotencyKey: "output-1" })
        } catch (error) {
          failure = error
        }
      },
    })
    const start = actorStart(0n, 0n)
    const output: Uint8Array[] = []
    const appendWritten = deferred<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(runProto.ProgramStartSchema, start)
      yield frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start))
      await appendWritten.promise
      const event = readEvent(output[1]!).event
      expect(event.case).toBe("actorOutputAppendRequested")
      if (event.case !== "actorOutputAppendRequested") return
      yield actorDecision(event.value.correlationId, "failed", JSON.stringify({
        code: "idempotency_conflict",
        message: "output key conflicts with an earlier append",
        retryable: false,
      }))
    }
    await runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => { if (output.length === 2) appendWritten.resolve() },
    }))
    expect(failure).toMatchObject({
      name: "HelmrError",
      code: "idempotency_conflict",
      message: "output key conflicts with an earlier append",
      retryable: false,
    })
  })

  test("sends Actor input from a Task with concurrent correlation-safe decisions", async () => {
    const mailbox = actor({ id: "mailbox", run() {} })
    let sent: unknown
    const definition = task({
      id: "deploy",
      async run() {
        sent = await Promise.all([
          mailbox.ref({ id: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa" }).input.send(
            { z: 1, a: 2 },
            { idempotencyKey: "\u0085first\u0085" },
          ),
          mailbox.ref({ key: "primary" }).input.send(null),
        ])
        return null
      },
    })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []
    const sendsWritten = deferred<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(runProto.ProgramStartSchema, start)
      yield frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start))
      await sendsWritten.promise
      const first = readEvent(output[1]!).event
      const second = readEvent(output[2]!).event
      expect(first.case).toBe("actorInputSendRequested")
      expect(second.case).toBe("actorInputSendRequested")
      if (first.case !== "actorInputSendRequested" ||
          second.case !== "actorInputSendRequested") return
      expect(first.value.declaredId).toBe("mailbox")
      expect(first.value.address).toEqual({
        case: "actorId",
        value: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa",
      })
      expect(first.value.dataJson).toBe('{"a":2,"z":1}')
      expect(first.value.idempotencyKey).toBe("first")
      expect(second.value.address).toEqual({ case: "actorKey", value: "primary" })
      expect(second.value.dataJson).toBe("null")
      yield actorDecision(second.value.correlationId, "completed", '{"sequence":2}')
      yield actorDecision(first.value.correlationId, "completed", '{"sequence":1}')
    }
    await runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => { if (output.length === 3) sendsWritten.resolve() },
    }))
    expect(sent).toEqual([{ sequence: 1 }, { sequence: 2 }])
    expect(output.map((frame) => readEvent(frame).event.case)).toEqual([
      "entrypointReady",
      "actorInputSendRequested",
      "actorInputSendRequested",
      "taskOutcome",
    ])
  })

  test("starts a detached child Task through the runtime protocol", async () => {
    const child = task({
      id: "resize-image",
      payload: {
        "~standard": {
          version: 1,
          vendor: "test",
          validate: (value: unknown) => ({ value }),
        },
      },
      run() {
        return null
      },
    })
    const definition = task({
      id: "deploy",
      async run() {
        return await child.start(
          { imageId: "image-1" },
          {
            workspace: workspaces.ref({ key: "child-workspace" }),
            idempotencyKey: "resize:image-1",
            queue: "priority",
            retry: {
              maxAttempts: 3,
              backoff: {
                minDelay: "1s",
                maxDelay: "30s",
                factor: 2,
                jitter: "full",
              },
            },
            metadata: { source: "parent" },
            tags: ["image"],
          },
        )
      },
    })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []
    const requested = deferred<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(runProto.ProgramStartSchema, start)
      yield frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start))
      await requested.promise
      const event = readEvent(output[1]!).event
      expect(event.case).toBe("taskChildInvokeRequested")
      if (event.case !== "taskChildInvokeRequested") return
      expect(event.value.declaredId).toBe("resize-image")
      expect(event.value.method).toBe("start")
      expect(event.value.payloadPresent).toBe(true)
      expect(event.value.payloadJson).toBe('{"imageId":"image-1"}')
      expect(event.value.workspaceJson).toBe('{"key":"child-workspace"}')
      expect(JSON.parse(event.value.optionsJson)).toEqual({
        metadata: { source: "parent" },
        queue: "priority",
        retry: {
          backoff: {
            factor: 2,
            jitter: "full",
            max_delay: "30s",
            min_delay: "1s",
          },
          max_attempts: 3,
        },
        tags: ["image"],
      })
      expect(event.value.idempotencyKey).toBe("resize:image-1")
      yield actorDecision(
        event.value.correlationId,
        "completed",
        '{"run_id":"run_aaaaaaaaaaaaaaaaaaaaaaaaaa"}',
      )
    }
    await runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => {
        if (output.length === 2) requested.resolve()
      },
    }))
    expect(output.map((frame) => readEvent(frame).event.case)).toEqual([
      "entrypointReady",
      "taskChildInvokeRequested",
      "taskOutcome",
    ])
    const outcome = readEvent(output[2]!).event
    expect(outcome.case).toBe("taskOutcome")
    if (outcome.case === "taskOutcome" && outcome.value.outcome.case === "succeeded") {
      expect(outcome.value.outcome.value.outputJson).toBe(
        '{"id":"run_aaaaaaaaaaaaaaaaaaaaaaaaaa"}',
      )
    }
  })

  test("preserves omitted Actor start input separately from JSON null", async () => {
    const mailbox = actor({ id: "mailbox", run() {} })
    const observed: Array<string | undefined> = []
    const definition = task({
      id: "deploy",
      async run() {
        await mailbox.start({
          workspace: workspaces.ref({ key: "actor-workspace" }),
        })
        await mailbox.start({
          workspace: workspaces.ref({ key: "actor-workspace" }),
          input: null,
        })
        return null
      },
    })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []
    const requestWritten = [deferred<void>(), deferred<void>()]
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(runProto.ProgramStartSchema, start)
      yield frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start))
      for (let index = 0; index < 2; index++) {
        await requestWritten[index]!.promise
        const event = readEvent(output[index + 1]!).event
        if (event.case !== "actorStartRequested") return
        observed.push(event.value.inputJson)
        yield actorDecision(
          event.value.correlationId,
          "completed",
          '{"actor_id":"act_aaaaaaaaaaaaaaaaaaaaaaaaaa","run_id":"run_aaaaaaaaaaaaaaaaaaaaaaaaaa"}',
        )
      }
    }
    await runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => {
        if (output.length === 2) requestWritten[0]!.resolve()
        if (output.length === 3) requestWritten[1]!.resolve()
      },
    }))
    expect(observed).toEqual([undefined, "null"])
    expect(output.map((frame) => readEvent(frame).event.case)).toEqual([
      "entrypointReady",
      "actorStartRequested",
      "actorStartRequested",
      "taskOutcome",
    ])
  })

  test("bridges every Workspace runtime operation through typed events", async () => {
    const cache = workspace("cache")
      .image(image("root").from("debian:bookworm-slim"))
      .resources({ cpu: 1, memory: "1GiB" })
    const observed: string[] = []
    const definition = task({
      id: "deploy",
      async run() {
        const created = await cache.create({
          key: "build-cache",
          secrets: [{ name: "TOKEN", env: "TOKEN" }],
          idempotencyKey: "create:cache",
        })
        const snapshot = await created.retrieve()
        const content = await created.files.read("result.txt")
        const entry = await created.files.stat("result.txt")
        const page = await created.files.list(".", { limit: 10 })
        const executed = await created.exec({
          command: ["sh", "-c", "printf ok"],
          stdin: new Uint8Array([1, 2, 3]),
          timeout: "1s",
          idempotencyKey: "exec:cache",
        })
        const deleted = await created.delete({ idempotencyKey: "delete:cache" })
        return {
          id: snapshot.id,
          content: new TextDecoder().decode(content),
          kind: entry.kind,
          count: page.items.length,
          exitCode: executed.exitCode,
          stdout: new TextDecoder().decode(executed.stdout),
          deleted: deleted.workspaceId,
        }
      },
    })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []
    const requested = Array.from({ length: 7 }, () => deferred<void>())
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(runProto.ProgramStartSchema, start)
      yield frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start))
      const responses = [
        '{"workspace_id":"wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa"}',
        '{"id":"wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa","key":"build-cache","declared_id":"cache","status":"available","secrets":[{"name":"TOKEN","env":"TOKEN"}],"last_activity_at":"2026-07-26T00:00:00Z","created_at":"2026-07-26T00:00:00Z","updated_at":"2026-07-26T00:00:00Z"}',
        '{"data_base64":"b2s="}',
        '{"path":"result.txt","kind":"file","mode":420,"size_bytes":2}',
        '{"items":[{"path":"result.txt","kind":"file","mode":420,"size_bytes":2}]}',
        '{"exit_code":0,"stdout_base64":"b2s=","stderr_base64":""}',
        '{"workspace_id":"wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa"}',
      ]
      for (let index = 0; index < responses.length; index++) {
        await requested[index]!.promise
        const event = readEvent(output[index + 1]!).event
        if (event.case === undefined) return
        observed.push(event.case)
        const correlationId = "correlationId" in event.value
          ? event.value.correlationId as string
          : ""
        yield actorDecision(correlationId, "completed", responses[index]!)
      }
    }
    await runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => {
        const index = output.length - 2
        if (index >= 0 && index < requested.length) requested[index]!.resolve()
      },
    }))
    expect(observed).toEqual([
      "workspaceCreateRequested",
      "workspaceRetrieveRequested",
      "workspaceFileReadRequested",
      "workspaceFileStatRequested",
      "workspaceFileListRequested",
      "workspaceExecRequested",
      "workspaceDeleteRequested",
    ])
    const outcome = readEvent(output.at(-1)!).event
    expect(outcome.case).toBe("taskOutcome")
    if (outcome.case === "taskOutcome" &&
      outcome.value.outcome.case === "succeeded") {
      expect(outcome.value.outcome.value.outputJson).toBe(
        '{"content":"ok","count":1,"deleted":"wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa","exitCode":0,"id":"wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa","kind":"file","stdout":"ok"}',
      )
    }
  })

  test("calls a parent-owned child Task and returns its durable result", async () => {
    const child = task({
      id: "resize-image",
      payload: {
        "~standard": {
          version: 1,
          vendor: "test",
          validate: (value: unknown) => ({ value }),
        },
      },
      run() {
        return { resized: true }
      },
    })
    let result: unknown
    let overlappingWaitError = ""
    const definition = actor({
      id: "worker",
      async run() {
        const called = child.call(
          { imageId: "image-1" },
          {
            workspace: workspaces.ref({ key: "child-workspace" }),
            idempotencyKey: "resize:image-1",
          },
        )
        try {
          await timers.waitFor("1m")
        } catch (error) {
          overlappingWaitError =
            error instanceof Error ? error.message : String(error)
        }
        result = await called
      },
    })
    const start = actorStart(4n, 4n)
    const output: Uint8Array[] = []
    const requested = deferred<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(runProto.ProgramStartSchema, start)
      yield frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start))
      await requested.promise
      const event = readEvent(output[1]!).event
      expect(event.case).toBe("taskChildInvokeRequested")
      if (event.case !== "taskChildInvokeRequested") return
      expect(event.value.method).toBe("call")
      expect(event.value.actorSpeculativeInputSequence).toBe(4n)
      expect(event.value.idempotencyKey).toBe("resize:image-1")
      yield actorDecision(
        event.value.correlationId,
        "completed",
        JSON.stringify({
          ok: true,
          output: { resized: true },
          run: { id: "run_aaaaaaaaaaaaaaaaaaaaaaaaaa" },
        }),
      )
    }
    await runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => {
        if (output.length === 2) requested.resolve()
      },
    }))
    expect(overlappingWaitError).toBe("only one consuming Wait may be pending")
    expect(result).toEqual({
      ok: true,
      output: { resized: true },
      run: { id: "run_aaaaaaaaaaaaaaaaaaaaaaaaaa" },
    })
    expect(output.map((frame) => readEvent(frame).event.case)).toEqual([
      "entrypointReady",
      "taskChildInvokeRequested",
      "actorOutcome",
    ])
  })

  test("task.call unwrap throws the recorded remote RunError", async () => {
    const child = task({ id: "resize-image", run: () => null })
    let failure: unknown
    const definition = task({
      id: "deploy",
      async run() {
        try {
          await child.call({
            workspace: workspaces.ref({ key: "child-workspace" }),
            idempotencyKey: "resize:image-1",
          }).unwrap()
        } catch (error) {
          failure = error
        }
        return null
      },
    })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []
    const requested = deferred<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(runProto.ProgramStartSchema, start)
      yield frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start))
      await requested.promise
      const event = readEvent(output[1]!).event
      if (event.case !== "taskChildInvokeRequested") return
      expect(event.value.method).toBe("call")
      expect(event.value.actorSpeculativeInputSequence).toBeUndefined()
      yield actorDecision(
        event.value.correlationId,
        "completed",
        JSON.stringify({
          ok: false,
          error: {
            code: "task_failed",
            message: "resize failed",
            retryable: false,
            details: { stage: "decode" },
          },
          run: { id: "run_bbbbbbbbbbbbbbbbbbbbbbbbbb" },
        }),
      )
    }
    await runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => {
        if (output.length === 2) requested.resolve()
      },
    }))
    expect(failure).toBeInstanceOf(Error)
    expect(failure).toMatchObject({
      name: "HelmrError",
      message: "resize failed",
      code: "task_failed",
      retryable: false,
      details: { stage: "decode" },
    })
  })

  test("creates and waits for an externally completed Token", async () => {
    let completed: unknown
    const definition = task({
      id: "deploy",
      async run() {
        const token = await tokens.create({
          timeout: "10m",
          metadata: { approval: true },
          tags: ["review"],
          idempotencyKey: "approval-1",
        })
        completed = await token.wait({
          timeout: "30m",
          idleTimeout: "45s",
          metadata: { stage: "approval" },
          tags: ["human"],
        }).unwrap()
        return null
      },
    })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []
    const createWritten = deferred<void>()
    const waitWritten = deferred<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(runProto.ProgramStartSchema, start)
      yield frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start))
      await createWritten.promise
      const createEvent = readEvent(output[1]!).event
      expect(createEvent.case).toBe("tokenCreateRequested")
      if (createEvent.case !== "tokenCreateRequested") return
      expect(createEvent.value.timeoutMs).toBe(600_000n)
      expect(createEvent.value.idempotencyKey).toBe("approval-1")
      expect(createEvent.value.metadataJson).toBe('{"approval":true}')
      expect(createEvent.value.tags).toEqual(["review"])
      yield actorDecision(
        createEvent.value.correlationId,
        "completed",
        JSON.stringify({
          id: "tok_aaaaaaaaaaaaaaaaaaaaaaaaaa",
          status: "pending",
          callback_url: "https://api.example.test/callback",
          public_access_token: "hlmr_pat_secret",
          timeout_at: "2026-07-24T12:00:00Z",
          metadata: { approval: true },
          tags: ["review"],
          created_at: "2026-07-24T11:50:00Z",
          updated_at: "2026-07-24T11:50:00Z",
        }),
      )
      await waitWritten.promise
      const waitEvent = readEvent(output[2]!).event
      expect(waitEvent.case).toBe("runWaitRequested")
      if (waitEvent.case !== "runWaitRequested") return
      expect(waitEvent.value.kind).toBe("token")
      expect(waitEvent.value.timeoutMs).toBe(1_800_000n)
      expect(waitEvent.value.idleTimeoutMs).toBe(45_000n)
      expect(waitEvent.value.metadataJson).toBe('{"stage":"approval"}')
      expect(waitEvent.value.tags).toEqual(["human"])
      expect(JSON.parse(waitEvent.value.paramsJson)).toEqual({
        token_id: "tok_aaaaaaaaaaaaaaaaaaaaaaaaaa",
      })
      yield actorDecision(
        waitEvent.value.correlationId,
        "completed",
        '{"approved":true}',
      )
    }
    await runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => {
        if (output.length === 2) createWritten.resolve()
        if (output.length === 3) waitWritten.resolve()
      },
    }))
    expect(completed).toEqual({ approved: true })
    expect(output.map((frame) => readEvent(frame).event.case)).toEqual([
      "entrypointReady",
      "tokenCreateRequested",
      "runWaitRequested",
      "taskOutcome",
    ])
  })

  test("emits acknowledged metadata mutations and structured logs", async () => {
    const definition = task({
      id: "deploy",
      async run() {
        await metadata.set("phase", "running")
        await metadata.increment("steps", 2)
        await logger.error("step failed", { step: 2, retryable: false })
        return null
      },
    })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []
    const writes = [deferred<void>(), deferred<void>(), deferred<void>()]
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(runProto.ProgramStartSchema, start)
      yield frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start))

      await writes[0]!.promise
      const set = readEvent(output[1]!).event
      expect(set.case).toBe("metadataUpdated")
      if (set.case !== "metadataUpdated") return
      expect(set.value).toMatchObject({
        operation: "set",
        key: "phase",
        valueJson: '"running"',
      })
      yield actorDecision(set.value.correlationId, "completed", "{}")

      await writes[1]!.promise
      const increment = readEvent(output[2]!).event
      expect(increment.case).toBe("metadataUpdated")
      if (increment.case !== "metadataUpdated") return
      expect(increment.value).toMatchObject({
        operation: "increment",
        key: "steps",
        amount: 2,
      })
      yield actorDecision(increment.value.correlationId, "completed", "{}")

      await writes[2]!.promise
      const log = readEvent(output[3]!).event
      expect(log.case).toBe("structuredLogRequested")
      if (log.case !== "structuredLogRequested") return
      expect(log.value).toMatchObject({
        level: "error",
        message: "step failed",
      })
      expect(JSON.parse(log.value.attributesJson)).toEqual({
        retryable: false,
        step: 2,
      })
      yield actorDecision(log.value.correlationId, "completed", "{}")
    }
    await runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => {
        if (output.length >= 2 && output.length <= 4) {
          writes[output.length - 2]!.resolve()
        }
      },
    }))
    expect(readEvent(output[4]!).event.case).toBe("taskOutcome")
  })

  test.each(["0.5s", "01s", " 1s"])(
    "rejects non-canonical Token duration %s before emission",
    async (timeout) => {
      let caught: unknown
      const definition = task({
        id: "deploy",
        async run() {
          try {
            await tokens.create({ timeout: timeout as never })
          } catch (error) {
            caught = error
          }
          return null
        },
      })
      const start = taskStart("noPayload")
      const output: Uint8Array[] = []
      await runProgram(locatorURL, programIO({
        input: frames(
          frameMessage(runProto.ProgramStartSchema, start),
          frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start)),
        ),
        definition,
        output,
      }))
      expect(caught).toBeInstanceOf(Error)
      expect((caught as Error).message).toContain("positive integer")
      expect(output.map((frame) => readEvent(frame).event.case)).toEqual([
        "entrypointReady",
        "taskOutcome",
      ])
    },
  )

  test("rejects an oversized Actor input idempotency key before emission", async () => {
    const mailbox = actor({ id: "mailbox", run() {} })
    let failure: unknown
    const definition = task({
      id: "deploy",
      async run() {
        try {
          await mailbox.ref({ key: "primary" }).input.send(null, {
            idempotencyKey: "a".repeat(513),
          })
        } catch (error) {
          failure = error
        }
        return null
      },
    })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []
    await runProgram(locatorURL, programIO({
      input: frames(
        frameMessage(runProto.ProgramStartSchema, start),
        frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start)),
      ),
      definition,
      output,
    }))
    expect(failure).toMatchObject({
      name: "HelmrError",
      code: "invalid_idempotency_key",
      retryable: false,
    })
    expect(output.map((frame) => readEvent(frame).event.case)).toEqual([
      "entrypointReady",
      "taskOutcome",
    ])
  })

  test("surfaces an Actor input send semantic failure to Actor user code", async () => {
    const mailbox = actor({ id: "mailbox", run() {} })
    let failure: unknown
    const definition = actor({
      id: "worker",
      async run() {
        try {
          await mailbox.ref({ key: "closed" }).input.send({ hello: "world" })
        } catch (error) {
          failure = error
        }
      },
    })
    const start = actorStart(0n, 0n)
    const output: Uint8Array[] = []
    const sendWritten = deferred<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(runProto.ProgramStartSchema, start)
      yield frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start))
      await sendWritten.promise
      const send = readEvent(output[1]!).event
      if (send.case !== "actorInputSendRequested") return
      yield actorDecision(send.value.correlationId, "failed", JSON.stringify({
        code: "actor_not_open",
        message: "Actor does not accept new input",
        retryable: false,
      }))
    }
    await runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => { if (output.length === 2) sendWritten.resolve() },
    }))
    expect(failure).toMatchObject({
      name: "HelmrError",
      code: "actor_not_open",
      retryable: false,
      message: "Actor does not accept new input",
    })
    expect(readEvent(output[2]!).event.case).toBe("actorOutcome")
  })

  test("does not emit a pre-aborted send and drains a post-emission abort", async () => {
    const mailbox = actor({ id: "mailbox", run() {} })
    const preAborted = new AbortController()
    preAborted.abort(new DOMException("pre-aborted", "AbortError"))
    const postEmission = new AbortController()
    const failures: unknown[] = []
    const definition = task({
      id: "deploy",
      async run() {
        for (const signal of [preAborted.signal, postEmission.signal]) {
          try {
            await mailbox.ref({ key: "primary" }).input.send(null, { signal })
          } catch (error) {
            failures.push(error)
          }
        }
        return null
      },
    })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []
    const sendWritten = deferred<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(runProto.ProgramStartSchema, start)
      yield frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start))
      await sendWritten.promise
      const send = readEvent(output[1]!).event
      if (send.case !== "actorInputSendRequested") return
      yield actorDecision(send.value.correlationId, "completed", '{"sequence":3}')
    }
    await runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => {
        if (output.length === 2) {
          postEmission.abort(new DOMException("post-emission", "AbortError"))
          sendWritten.resolve()
        }
      },
    }))
    expect(failures).toHaveLength(2)
    expect(failures.map((failure) =>
      failure instanceof Error ? failure.message : String(failure)
    )).toEqual(["pre-aborted", "post-emission"])
    expect(output.map((frame) => readEvent(frame).event.case)).toEqual([
      "entrypointReady",
      "actorInputSendRequested",
      "taskOutcome",
    ])
  })

  test("waits for the exact entrypoint release before invoking a payload-free task", async () => {
    let invoked = false
    const definition = task({
      id: "deploy",
      run(ctx) {
        invoked = true
        return { runId: ctx.run.id }
      },
    })
    const start = taskStart("noPayload")
    const release = releaseFor(start)
    const gate = deferred<void>()
    const ready = deferred<void>()
    const output: Uint8Array[] = []
    const running = runProgram(locatorURL, programIO({
      input: gatedFrames(frameMessage(runProto.ProgramStartSchema, start), gate.promise, frameMessage(runProto.EntrypointReleaseSchema, release)),
      definition,
      output,
      onWrite: () => ready.resolve(),
    }))

    await ready.promise
    expect(invoked).toBe(false)
    expect(readEvent(output[0]!).event.case).toBe("entrypointReady")

    gate.resolve()
    await running
    expect(invoked).toBe(true)
    const result = readEvent(output[1]!).event
    expect(result.case).toBe("taskOutcome")
    if (result.case === "taskOutcome") {
      expect(result.value.outcome.case).toBe("succeeded")
      if (result.value.outcome.case === "succeeded") {
        expect(result.value.outcome.value.outputJson).toBe('{"runId":"run-1"}')
      }
    }
  })

  test("preserves JSON null and delays payload validation until release", async () => {
    let validated = false
    let received: unknown
    const definition = task({
      id: "deploy",
      payload: {
        "~standard": {
          version: 1,
          vendor: "test",
          validate(value: unknown) {
            validated = true
            return { value }
          },
        },
      },
      run(payload) {
        received = payload
        return null
      },
    })
    const start = taskStart("payloadJson", new TextEncoder().encode("null"))
    const gate = deferred<void>()
    const ready = deferred<void>()
    const output: Uint8Array[] = []
    const running = runProgram(locatorURL, programIO({
      input: gatedFrames(
        frameMessage(runProto.ProgramStartSchema, start),
        gate.promise,
        frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start)),
      ),
      definition,
      output,
      onWrite: () => ready.resolve(),
    }))

    await ready.promise
    expect(validated).toBe(false)
    expect(received).toBeUndefined()
    gate.resolve()
    await running
    expect(validated).toBe(true)
    expect(received).toBeNull()
    const result = readEvent(output[1]!).event
    expect(result.case).toBe("taskOutcome")
    if (result.case === "taskOutcome") {
      expect(result.value.outcome.case).toBe("succeeded")
      if (result.value.outcome.case === "succeeded") {
        expect(result.value.outcome.value.outputJson).toBe("null")
      }
    }
  })

  test("emits one timer Wait and consumes only its matching decision", async () => {
    const definition = task({
      id: "deploy",
      async run() {
        await timers.waitFor("1m")
        return { resumed: true }
      },
    })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []
    const waitWritten = deferred<void>()
		let correlationId = ""
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(runProto.ProgramStartSchema, start)
      yield frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start))
      await waitWritten.promise
      const event = readEvent(output[1]!).event
      expect(event.case).toBe("runWaitRequested")
      if (event.case !== "runWaitRequested") return
			correlationId = event.value.correlationId
      expect(event.value.kind).toBe("timer")
      expect(event.value.timeoutMs).toBe(60_000n)
      expect(JSON.parse(event.value.paramsJson)).toEqual({ duration: "1m" })
      yield frameMessage(runProto.ResumeDecisionSchema, create(
        runProto.ResumeDecisionSchema,
        {
			runWaitId: "durable-wait-1",
			correlationId: event.value.correlationId,
          kind: "completed",
          dataJson: "null",
			requireConsumedAck: true,
			checkpointId: "checkpoint-1",
			resumeAttachId: "attach-1",
			resumeRequestVersion: 4n,
			runLeaseId: "lease-2",
        },
      ))
    }

    await runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => {
        if (output.length === 2) waitWritten.resolve()
      },
    }))

		const consumed = readEvent(output[2]!).event
		expect(consumed.case).toBe("resumeConsumed")
		if (consumed.case === "resumeConsumed") {
			expect(consumed.value.runWaitId).toBe("durable-wait-1")
			expect(consumed.value.resumeRequestVersion).toBe(4n)
			expect(consumed.value.correlationId).toBe(correlationId)
		}

		const result = readEvent(output[3]!).event
    expect(result.case).toBe("taskOutcome")
    if (result.case === "taskOutcome" && result.value.outcome.case === "succeeded") {
      expect(result.value.outcome.value.outputJson).toBe('{"resumed":true}')
    }
  })

  test("classifies a throwing payload schema as a bounded non-retryable validation failure", async () => {
    let invoked = false
    const definition = task({
      id: "deploy",
      payload: {
        "~standard": {
          version: 1,
          vendor: "test",
          validate() {
            throw new Error("x".repeat(4_096))
          },
        },
      },
      run() {
        invoked = true
        return null
      },
    })
    const start = taskStart("payloadJson", new TextEncoder().encode("{}"))
    const output: Uint8Array[] = []

    await runProgram(locatorURL, programIO({
      input: frames(
        frameMessage(runProto.ProgramStartSchema, start),
        frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start)),
      ),
      definition,
      output,
    }))

    expect(invoked).toBe(false)
    const result = readEvent(output[1]!).event
    expect(result.case).toBe("taskOutcome")
    if (result.case === "taskOutcome") {
      expect(result.value.outcome.case).toBe("payloadInvalid")
      if (result.value.outcome.case === "payloadInvalid") {
        expect(result.value.outcome.value.message).toBe("task payload failed validation")
        const details = JSON.parse(result.value.outcome.value.detailsJson!)
        expect(details.message).toEndWith("…")
        expect(new TextEncoder().encode(details.message).byteLength).toBeLessThanOrEqual(2_048)
      }
    }
  })

  test("does not translate a payload outcome transport failure", async () => {
    const definition = task({
      id: "deploy",
      payload: {
        "~standard": {
          version: 1,
          vendor: "test",
          validate: () => ({ issues: [{ message: "invalid" }] }),
        },
      },
      run: () => null,
    })
    const start = taskStart("payloadJson", new TextEncoder().encode("{}"))
    const output: Uint8Array[] = []

    await expect(runProgram(locatorURL, programIO({
      input: frames(
        frameMessage(runProto.ProgramStartSchema, start),
        frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start)),
      ),
      definition,
      output,
      failWriteAt: 2,
    }))).rejects.toThrow("closed control stream")
    expect(output).toHaveLength(1)
  })

  test("reports handler failures without granting retry authority", async () => {
    const definition = task({
      id: "deploy",
      run() {
        throw new Error("猫".repeat(1_000))
      },
    })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []

    await runProgram(locatorURL, programIO({
      input: frames(
        frameMessage(runProto.ProgramStartSchema, start),
        frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start)),
      ),
      definition,
      output,
    }))

    const result = readEvent(output[1]!).event
    expect(result.case).toBe("taskOutcome")
    if (result.case === "taskOutcome") {
      expect(result.value.outcome.case).toBe("failed")
      if (result.value.outcome.case === "failed") {
        expect(result.value.outcome.value.message).toEndWith("…")
        expect(
          new TextEncoder().encode(result.value.outcome.value.message).byteLength,
        ).toBeLessThanOrEqual(1_024)
        expect(result.value.outcome.value.detailsJson).toBeUndefined()
      }
    }
  })

  test("reports non-JSON and oversized handler outputs as Task failures", async () => {
    for (const value of [undefined, "x".repeat(16 * 1024 * 1024)]) {
      const definition = task({
        id: "deploy",
        run: (() => value) as () => never,
      })
      const start = taskStart("noPayload")
      const output: Uint8Array[] = []

      await runProgram(locatorURL, programIO({
        input: frames(
          frameMessage(runProto.ProgramStartSchema, start),
          frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start)),
        ),
        definition,
        output,
      }))
      const result = readEvent(output[1]!).event
      expect(result.case).toBe("taskOutcome")
      if (result.case === "taskOutcome") {
        expect(result.value.outcome.case).toBe("failed")
      }
    }
  })

  test("does not translate an outcome transport failure into another outcome", async () => {
    const definition = task({ id: "deploy", run: () => null })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []

    await expect(runProgram(locatorURL, programIO({
      input: frames(
        frameMessage(runProto.ProgramStartSchema, start),
        frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start)),
      ),
      definition,
      output,
      failWriteAt: 2,
    }))).rejects.toThrow("closed control stream")
    expect(output).toHaveLength(1)
  })

  test("reads frames split into single-byte chunks", async () => {
    let invoked = false
    const definition = task({
      id: "deploy",
      run() {
        invoked = true
        return null
      },
    })
    const start = taskStart("noPayload")
    const input = concatenate(
      frameMessage(runProto.ProgramStartSchema, start),
      frameMessage(runProto.EntrypointReleaseSchema, releaseFor(start)),
    )

    await runProgram(locatorURL, programIO({
      input: byteFrames(input),
      definition,
      output: [],
    }))

    expect(invoked).toBe(true)
  })

  test("rejects a mismatched branded export before entrypoint ready", async () => {
    let invoked = false
    const definition = task({
      id: "other",
      run() {
        invoked = true
        return null
      },
    })
    const output: Uint8Array[] = []

    await expect(
      runProgram(locatorURL, programIO({
        input: frames(frameMessage(runProto.ProgramStartSchema, taskStart("noPayload"))),
        definition,
        output,
      })),
    ).rejects.toThrow("does not match")
    expect(output).toHaveLength(0)
    expect(invoked).toBe(false)
  })

  test("rejects malformed and oversized frames before declaration import", async () => {
    let imported = false
    const malformed = frame(new Uint8Array([0xff]))
    const oversized = new Uint8Array(4)
    new DataView(oversized.buffer).setUint32(0, 256 * 1024 * 1024 + 1)

    for (const input of [malformed, oversized]) {
      await expect(
        runProgram(locatorURL, {
          input: frames(input),
          write: async () => {},
          readLocator: async () => {
            throw new Error("locator must not be read")
          },
          importModule: async () => {
            imported = true
            return {}
          },
        }),
      ).rejects.toThrow()
    }
    expect(imported).toBe(false)
  })

  test("rejects an identity-mismatched release without invoking the handler", async () => {
    let invoked = false
    const definition = task({
      id: "deploy",
      run() {
        invoked = true
        return null
      },
    })
    const start = taskStart("noPayload")
    const wrong = create(runProto.EntrypointReleaseSchema, {
      runId: start.runId,
      attemptNumber: start.attemptNumber + 1,
      entrypoint: taskIdentity("deploy"),
    })

    await expect(
      runProgram(locatorURL, programIO({
        input: frames(
          frameMessage(runProto.ProgramStartSchema, start),
          frameMessage(runProto.EntrypointReleaseSchema, wrong),
        ),
        definition,
        output: [],
      })),
    ).rejects.toThrow("does not match")
    expect(invoked).toBe(false)
  })
})

function programIO(options: {
  readonly input: AsyncIterable<Uint8Array>
  readonly definition: unknown
  readonly output: Uint8Array[]
  readonly onWrite?: () => void
  readonly failWriteAt?: number
}): ProgramIO {
  let writeCount = 0
  return {
    input: options.input,
    readLocator: async () =>
      JSON.stringify({
        declarations: [
          {
            declaredId: "deploy",
            exportName: "definition",
            kind: "task",
            modulePath: "tasks/deploy.ts",
          },
          {
            declaredId: "worker",
            exportName: "definition",
            kind: "actor",
            modulePath: "actors/worker.ts",
          },
        ],
        formatVersion: 0,
      }),
    importModule: async () => ({ definition: options.definition }),
    write: async (value) => {
      writeCount++
      if (writeCount === options.failWriteAt) {
        throw new Error("closed control stream")
      }
      options.output.push(value)
      options.onWrite?.()
    },
  }
}

function taskStart(
  payload: "noPayload" | "payloadJson",
  value = new Uint8Array(),
): runProto.ProgramStart {
  return create(runProto.ProgramStartSchema, {
    entrypointDeclaredId: "deploy",
    runId: "run-1",
    attemptNumber: 1,
    cause: create(runProto.RunCauseSchema, {
      kind: {
        case: "api",
        value: create(runProto.ApiCauseSchema),
      },
    }),
    deploymentId: "deployment-1",
    deploymentVersion: "v1",
    workspaceId: "workspace-1",
    baseWorkspaceVersionId: "version-1",
    entrypoint: {
      case: "task",
      value: create(runProto.TaskStartSchema, {
        payload: payload === "noPayload"
          ? {
              case: "noPayload",
              value: create(runProto.NoPayloadSchema),
            }
          : {
              case: "payloadJson",
              value,
            },
      }),
    },
  })
}

function actorStart(start: bigint, highWatermark: bigint): runProto.ProgramStart {
  return create(runProto.ProgramStartSchema, {
    entrypointDeclaredId: "worker",
    runId: "run-1",
    attemptNumber: 1,
    cause: create(runProto.RunCauseSchema, {
      kind: { case: "actorStart", value: create(runProto.ActorStartCauseSchema) },
    }),
    deploymentId: "deployment-1",
    deploymentVersion: "v1",
    workspaceId: "workspace-1",
    baseWorkspaceVersionId: "version-1",
    entrypoint: {
      case: "actor",
      value: create(runProto.ActorStartSchema, {
        actorId: "actor-1",
        startInputSequence: start,
        inputHighWatermark: highWatermark,
      }),
    },
  })
}

function releaseFor(start: runProto.ProgramStart): runProto.EntrypointRelease {
  return create(runProto.EntrypointReleaseSchema, {
    runId: start.runId,
    attemptNumber: start.attemptNumber,
    entrypoint: start.entrypoint.case === "actor"
      ? actorIdentity(start.entrypointDeclaredId)
      : taskIdentity(start.entrypointDeclaredId),
  })
}

function actorIdentity(declaredId: string): runProto.EntrypointIdentity {
  return create(runProto.EntrypointIdentitySchema, {
    declaredId,
    kind: { case: "actor", value: create(runProto.ActorEntrypointSchema) },
  })
}

function taskIdentity(declaredId: string): runProto.EntrypointIdentity {
  return create(runProto.EntrypointIdentitySchema, {
    declaredId,
    kind: {
      case: "task",
      value: create(runProto.TaskEntrypointSchema),
    },
  })
}

function frameMessage<T extends { $typeName: string }>(
  schema: GenMessage<T>,
  message: T,
): Uint8Array {
  return frame(toBinary(schema, message))
}

function actorDecision(
  correlationId: string,
  kind: string,
  dataJson: string,
): Uint8Array {
  return frameMessage(runProto.ResumeDecisionSchema, create(
    runProto.ResumeDecisionSchema,
    { correlationId, kind, dataJson },
  ))
}

function actorInput(sequence: number, value: unknown): string {
  return JSON.stringify({
    value,
    record: {
      id: `record-${sequence}`,
      sequence,
      created_at: "2026-07-22T00:00:00Z",
      source: { type: "external" },
    },
  })
}

function frame(body: Uint8Array): Uint8Array {
  const result = new Uint8Array(body.byteLength + 4)
  new DataView(result.buffer).setUint32(0, body.byteLength)
  result.set(body, 4)
  return result
}

function readEvent(value: Uint8Array): runProto.RunEvent {
  const length = new DataView(
    value.buffer,
    value.byteOffset,
    value.byteLength,
  ).getUint32(0)
  expect(length).toBe(value.byteLength - 4)
  return fromBinary(runProto.RunEventSchema, value.subarray(4))
}

async function* frames(...values: Uint8Array[]): AsyncIterable<Uint8Array> {
  for (const value of values) yield value
}

async function* byteFrames(value: Uint8Array): AsyncIterable<Uint8Array> {
  for (const byte of value) yield new Uint8Array([byte])
}

function concatenate(...values: Uint8Array[]): Uint8Array {
  const length = values.reduce((sum, value) => sum + value.byteLength, 0)
  const result = new Uint8Array(length)
  let offset = 0
  for (const value of values) {
    result.set(value, offset)
    offset += value.byteLength
  }
  return result
}

async function* gatedFrames(
  first: Uint8Array,
  gate: Promise<void>,
  second: Uint8Array,
): AsyncIterable<Uint8Array> {
  yield first
  await gate
  yield second
}

function deferred<T>(): {
  readonly promise: Promise<T>
  readonly resolve: (value: T) => void
} {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((done) => {
    resolve = done
  })
  return { promise, resolve }
}
