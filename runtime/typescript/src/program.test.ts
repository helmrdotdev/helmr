import { create, fromBinary, toBinary, type GenMessage } from "@bufbuild/protobuf"
import { runProto } from "@helmr/proto"
import { actor, task, timers } from "@helmr/sdk"
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
        received.push(await self.input.receive().unwrap())
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
      expect(event.value.timeout).toBe(60)
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
            declaredId: "events",
            exportName: "events",
            kind: "run_stream",
            modulePath: "streams/events.ts",
          },
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
