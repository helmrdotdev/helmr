import { create, fromBinary, toBinary, type GenMessage } from "@bufbuild/protobuf"
import { programProto } from "@helmr/proto"
import { spawn } from "node:child_process"
import {
  actor,
  image,
  logger,
  metadata,
  secrets,
  task,
  timers,
  tokens,
  sandbox,
  sessions,
  workspaces,
} from "@helmr/sdk"
import { describe, expect, test } from "bun:test"

import { runProgram, type ProgramIO } from "./program"

const locatorURL = new URL(
  "file:///opt/helmr/program/helmr/declarations.json",
)

describe("runProgram", () => {
  test("releases the owned input iterator after the terminal Task outcome", async () => {
    const definition = task({ id: "deploy", run: () => null })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []
    const lifecycle: string[] = []
    const input = observedFrames([
      frameMessage(programProto.ProgramStartSchema, start),
      frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start)),
    ], async () => {
      lifecycle.push("input-closed")
    })

    await runProgram(locatorURL, programIO({
      input,
      definition,
      output,
      onWrite: () => {
        lifecycle.push(readEvent(output.at(-1)!).event.case ?? "unknown")
      },
    }))

    expect(lifecycle).toEqual([
      "entrypointReady",
      "taskOutcome",
      "input-closed",
    ])
  })

  test("releases input after handled Task and Actor failures", async () => {
    const cases = [
      {
        definition: task({ id: "deploy", run() { throw new Error("task failed") } }),
        start: taskStart("noPayload"),
      },
      {
        definition: actor({ id: "worker", run() { throw new Error("actor failed") } }),
        start: actorStart(0n, 0n),
      },
    ] as const

    for (const item of cases) {
      let closeCount = 0
      const input = observedFrames([
        frameMessage(programProto.ProgramStartSchema, item.start),
        frameMessage(programProto.EntrypointReleaseSchema, releaseFor(item.start)),
      ], async () => {
        closeCount++
      })
      await runProgram(locatorURL, programIO({
        input,
        definition: item.definition,
        output: [],
      }))
      expect(closeCount).toBe(1)
    }
  })

  test("propagates input release failure after one terminal outcome", async () => {
    const definition = task({ id: "deploy", run: () => null })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []
    const input = observedFrames([
      frameMessage(programProto.ProgramStartSchema, start),
      frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start)),
    ], async () => {
      throw new Error("input release failed")
    })

    await expect(runProgram(locatorURL, programIO({
      input,
      definition,
      output,
    }))).rejects.toThrow("input release failed")
    expect(output.map((value) => readEvent(value).event.case)).toEqual([
      "entrypointReady",
      "taskOutcome",
    ])
  })

  test("does not release input on an exceptional protocol path", async () => {
    let closeCount = 0
    const start = taskStart("noPayload")
    const input = observedFrames([
      frameMessage(programProto.ProgramStartSchema, start),
    ], async () => {
      closeCount++
    })

    await expect(runProgram(locatorURL, programIO({
      input,
      definition: task({ id: "other", run: () => null }),
      output: [],
    }))).rejects.toThrow("does not match")
    expect(closeCount).toBe(0)
  })

  test("generated Runtime exits while its parent retains stdin", async () => {
    const start = taskStart("noPayload")
    const runtimeEntryURL = new URL(
      "../../../internal/runtime/entry.mjs",
      import.meta.url,
    ).href
    const childSource = `
      import { runProgram } from ${JSON.stringify(runtimeEntryURL)};
      const definition = {};
      Object.defineProperty(definition, Symbol.for("helmr.sdk.v0.definition"), {
        value: Object.freeze({
          kind: "task",
          id: "deploy",
          hasPayload: false,
          handler: () => null
        })
      });
      await runProgram(new URL("file:///opt/helmr/program/helmr/declarations.json"), {
        input: process.stdin,
        readLocator: async () => JSON.stringify({
          architecture: "x86_64",
          configResultDigest: "sha256:${"4".repeat(64)}",
          declarations: [{
            declaredId: "deploy",
            kind: "task",
            locator: {
              exportName: "definition",
              modulePath: ".helmr/modules/${"1".repeat(64)}.mjs",
              slot: "handler"
            },
            manifest: {}
          }],
          formatVersion: 0,
          queues: [],
          runtimeContract: "helmr.runtime.v0"
        }),
        importModule: async () => ({ definition }),
        write: async (frame) => { process.stdout.write(frame); }
      });
    `
    const child = spawn("node", ["--input-type=module", "--eval", childSource], {
      cwd: process.cwd(),
      stdio: ["pipe", "pipe", "pipe"],
    })
    const closed = new Promise<{
      code: number | null
      signal: NodeJS.Signals | null
    }>((resolve, reject) => {
      child.once("error", reject)
      child.once("close", (code, signal) => resolve({ code, signal }))
    })
    const output: Uint8Array[] = []
    child.stdout.on("data", (value: Buffer) => {
      output.push(new Uint8Array(value))
    })
    let stderr = ""
    child.stderr.setEncoding("utf8")
    child.stderr.on("data", (value: string) => {
      stderr += value
    })
    child.stdin.write(concatenate(
      frameMessage(programProto.ProgramStartSchema, start),
      frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start)),
    ))

    let timeout: ReturnType<typeof setTimeout> | undefined
    try {
      const result = await Promise.race([
        closed,
        new Promise<never>((_resolve, reject) => {
          timeout = setTimeout(() => {
            child.kill()
            reject(new Error("generated Runtime retained its parent stdin"))
          }, 5_000)
        }),
      ])
      expect({ result, stderr }).toEqual({
        result: { code: 0, signal: null },
        stderr: "",
      })
    } finally {
      if (timeout !== undefined) clearTimeout(timeout)
      child.stdin.destroy()
      if (child.exitCode === null && child.signalCode === null) child.kill()
      await closed.catch(() => {})
    }
    expect(readConcatenatedEvents(concatenate(...output)).map((event) =>
      event.event.case
    )).toEqual(["entrypointReady", "taskOutcome"])
  })

  test("reports an Actor return with its terminal cursor", async () => {
    let actorID = ""
    let sessionID = ""
    const definition = actor({
      id: "worker",
      run(session, ctx) {
        actorID = ctx.actor.id
        sessionID = session.id
      },
    })
    const start = actorStart(0n, 0n)
    const output: Uint8Array[] = []
    await runProgram(locatorURL, programIO({
      input: frames(
        frameMessage(programProto.ProgramStartSchema, start),
        frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start)),
      ),
      definition,
      output,
    }))
    expect(actorID).toBe("worker")
    expect(sessionID).toBe("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33")
    const outcome = readEvent(output[1]!).event
    expect(outcome.case).toBe("actorOutcome")
    if (outcome.case === "actorOutcome") {
      expect(outcome.value.terminalInputSequence).toBe(0n)
      expect(outcome.value.outcome.case).toBe("succeeded")
    }
  })

  test("reports an Actor throw as a bounded failure with its cursor", async () => {
    const definition = actor({ id: "worker", run() { throw new Error("\u0085boom\u0085") } })
    const start = actorStart(4n, 7n)
    const output: Uint8Array[] = []
    await runProgram(locatorURL, programIO({
      input: frames(
        frameMessage(programProto.ProgramStartSchema, start),
        frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start)),
      ),
      definition,
      output,
    }))
    const outcome = readEvent(output[1]!).event
    expect(outcome.case).toBe("actorOutcome")
    if (outcome.case === "actorOutcome") {
      expect(outcome.value.terminalInputSequence).toBe(4n)
      expect(outcome.value.outcome.case).toBe("failed")
      if (outcome.value.outcome.case === "failed") {
        expect(outcome.value.outcome.value.message).toBe("boom")
      }
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
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))

      await events[0]!.promise
      const first = readEvent(output[1]!).event
      expect(first.case).toBe("runWaitRequested")
      if (first.case !== "runWaitRequested") return
      expect(first.value.kind).toBe("actor_input")
      expect(JSON.parse(first.value.paramsJson).after_input_sequence).toBe(0)
      expect(first.value.actorSpeculativeInputSequence).toBe(0n)
      expect(first.value.timeoutMs).toBe(1n)
      expect(first.value.idleTimeoutMs).toBe(1501n)
      yield actorDecision(first.value.correlationId, "completed", actorInput(1, "one"), first.value)

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
      yield actorDecision(second.value.correlationId, "completed", actorInput(2, "two"), second.value)
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
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      await waitWritten.promise
      const wait = readEvent(output[1]!).event
      if (wait.case !== "runWaitRequested") return
      yield actorDecision(wait.value.correlationId, "completed", actorInput(1, "one"), wait.value)
    }
    await runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => { if (output.length === 2) waitWritten.resolve() },
    }))
    expect(overlapError).toBe("ConcurrentSessionReceiveError")
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
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      await waitWritten.promise
      const wait = readEvent(output[1]!).event
      if (wait.case !== "runWaitRequested") return
      yield actorDecision(wait.value.correlationId, "completed", actorInput(1, "one"), wait.value)
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
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      await waitWritten.promise
      const wait = readEvent(output[1]!).event
      if (wait.case !== "runWaitRequested") return
      yield actorDecision(
        wait.value.correlationId,
        "cancelled",
        JSON.stringify({ reason_code: "run_cancelled" }),
        wait.value,
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
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      await waitWritten.promise
      const wait = readEvent(output[1]!).event
      if (wait.case !== "runWaitRequested") return
      yield actorDecision(wait.value.correlationId, "completed", actorInput(2, "skipped"), wait.value)
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
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      await waitWritten.promise
      const wait = readEvent(output[1]!).event
      if (wait.case !== "runWaitRequested") return
      yield actorDecision(wait.value.correlationId, "completed", actorInput(2, "skipped"), wait.value)
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
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      await waitWritten.promise
      const wait = readEvent(output[1]!).event
      if (wait.case !== "runWaitRequested") return
      yield actorDecision(
        wait.value.correlationId,
        "cancelled",
        JSON.stringify({ reason_code: "run_cancelled" }),
        wait.value,
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
      JSON.stringify({
        id: "60af6067-a253-47b5-915c-2b889fb132c7",
        sequence: 1,
        data: "value",
        content_type: "application/json",
        created_at: "2026-07-22T00:00:00Z",
        provenance: {
          run_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc52",
          attempt_number: 1,
          deployment_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc53",
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
        yield frameMessage(programProto.ProgramStartSchema, start)
        yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
        await requestWritten.promise
        const request = readEvent(output[1]!).event
        const correlationId = request.case === "runWaitRequested"
          ? request.value.correlationId
          : request.case === "actorOutputAppendRequested"
          ? request.value.correlationId
          : ""
        yield actorDecision(
          correlationId,
          "completed",
          dataJson,
          request.case === "runWaitRequested" ? request.value : undefined,
        )
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
        frameMessage(programProto.ProgramStartSchema, start),
        frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start)),
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
        frameMessage(programProto.ProgramStartSchema, start),
        frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start)),
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
        frameMessage(programProto.ProgramStartSchema, start),
        frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start)),
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
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      await appendWritten.promise
      const event = readEvent(output[1]!).event
      expect(event.case).toBe("actorOutputAppendRequested")
      if (event.case !== "actorOutputAppendRequested") return
      expect(JSON.parse(event.value.dataJson)).toEqual({ event: "ready" })
      expect(event.value.contentType).toBe("application/vnd.helmr.test+json")
      expect(event.value.idempotencyKey).toBe("output-1")
      yield actorDecision(event.value.correlationId, "completed", JSON.stringify({
        id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc51",
        sequence: 1,
        data: { event: "ready" },
        content_type: event.value.contentType,
        created_at: "2026-07-22T00:00:00Z",
        provenance: {
          run_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc52",
          attempt_number: 1,
          deployment_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc53",
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
      id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc51",
      sequence: 1,
      data: { event: "ready" },
      contentType: "application/vnd.helmr.test+json",
      createdAt: "2026-07-22T00:00:00Z",
      provenance: {
        runId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc52",
        attemptNumber: 1,
        deploymentId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc53",
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
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
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
    })
    expect(Object.hasOwn(failure as object, "retryable")).toBe(false)
  })

  test("sends Actor input from a Task with concurrent correlation-safe decisions", async () => {
    const mailbox = actor({ id: "mailbox", run() {} })
    let sent: unknown
    const definition = task({
      id: "deploy",
      async run() {
        sent = await Promise.all([
          sessions.ref("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33").input.send(
            { z: 1, a: 2 },
            { idempotencyKey: "\u0085first\u0085" },
          ),
          sessions.ref("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33").input.send(null),
        ])
        return null
      },
    })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []
    const sendsWritten = deferred<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      await sendsWritten.promise
      const first = readEvent(output[1]!).event
      const second = readEvent(output[2]!).event
      expect(first.case).toBe("sessionInputSendRequested")
      expect(second.case).toBe("sessionInputSendRequested")
      if (first.case !== "sessionInputSendRequested" ||
          second.case !== "sessionInputSendRequested") return
      expect(first.value.sessionId).toBe(
        "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
      )
      expect(first.value.dataJson).toBe('{"a":2,"z":1}')
      expect(first.value.idempotencyKey).toBe("first")
      expect(second.value.sessionId).toBe(
        "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
      )
      expect(second.value.dataJson).toBe("null")
      yield actorDecision(second.value.correlationId, "completed", JSON.stringify({
        id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
        sequence: 2,
        data: null,
        source: {
          type: "run",
          run_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
        },
        created_at: "2026-07-26T00:00:01Z",
      }))
      yield actorDecision(first.value.correlationId, "completed", JSON.stringify({
        id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34",
        sequence: 1,
        data: { a: 2, z: 1 },
        source: {
          type: "run",
          run_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
        },
        created_at: "2026-07-26T00:00:00Z",
      }))
    }
    await runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => { if (output.length === 3) sendsWritten.resolve() },
    }))
    expect(sent).toEqual([
      {
        id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34",
        sequence: 1,
        data: { a: 2, z: 1 },
        source: {
          type: "run",
          runId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
        },
        createdAt: "2026-07-26T00:00:00Z",
      },
      {
        id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
        sequence: 2,
        data: null,
        source: {
          type: "run",
          runId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
        },
        createdAt: "2026-07-26T00:00:01Z",
      },
    ])
    expect(output.map((frame) => readEvent(frame).event.case)).toEqual([
      "entrypointReady",
      "sessionInputSendRequested",
      "sessionInputSendRequested",
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
            workspace: workspaces.ref("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"),
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
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      await requested.promise
      const event = readEvent(output[1]!).event
      expect(event.case).toBe("taskChildInvokeRequested")
      if (event.case !== "taskChildInvokeRequested") return
      expect(event.value.declaredId).toBe("resize-image")
      expect(event.value.method).toBe("start")
      expect(event.value.payloadPresent).toBe(true)
      expect(event.value.payloadJson).toBe('{"imageId":"image-1"}')
      expect(event.value.workspaceJson).toBe(
        '{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"}',
      )
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
        '{"run_id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"}',
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
        '{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"}',
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
          workspace: workspaces.ref("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"),
        })
        await mailbox.start({
          workspace: workspaces.ref("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"),
          input: null,
        })
        return null
      },
    })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []
    const requestWritten = [deferred<void>(), deferred<void>()]
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      for (let index = 0; index < 2; index++) {
        await requestWritten[index]!.promise
        const event = readEvent(output[index + 1]!).event
        if (event.case !== "actorStartRequested") return
        observed.push(event.value.inputJson)
        yield actorDecision(
          event.value.correlationId,
          "completed",
          '{"run_id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31","session_id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33"}',
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
    const cache = sandbox({ id: "cache" })
      .image(image("root").from("debian:bookworm-slim"))
      .resources({ cpu: 1, memory: "1GiB" })
    const observed: string[] = []
    const definition = task({
      id: "deploy",
      async run() {
        const created = await cache.createWorkspace({
          key: "build-cache",
          secrets: [{ secret: secrets.fromName("TOKEN"), env: "TOKEN" }],
          idempotencyKey: "create:cache",
        })
        const workspace = await created.retrieve()
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
          id: workspace.id,
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
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      const responses = [
        '{"workspace_id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"}',
        '{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32","key":"build-cache","sandbox_id":"cache","deployment_id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35","status":"available","secrets":[{"name":"TOKEN","env":"TOKEN"}],"last_activity_at":"2026-07-26T00:00:00Z","created_at":"2026-07-26T00:00:00Z","updated_at":"2026-07-26T00:00:00Z"}',
        '{"data_base64":"b2s="}',
        '{"path":"result.txt","kind":"file","mode":420,"size_bytes":2}',
        '{"items":[{"path":"result.txt","kind":"file","mode":420,"size_bytes":2}]}',
        '{"exit_code":0,"stdout_base64":"b2s=","stderr_base64":""}',
        '{"workspace_id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"}',
      ]
      for (let index = 0; index < responses.length; index++) {
        await requested[index]!.promise
        const event = readEvent(output[index + 1]!).event
        if (event.case === undefined) return
        observed.push(event.case)
        if (event.case === "workspaceCreateRequested") {
          expect(event.value.secrets).toMatchObject([{
            name: "TOKEN",
            placement: { case: "env", value: "TOKEN" },
          }])
        }
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
        '{"content":"ok","count":1,"deleted":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32","exitCode":0,"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32","kind":"file","stdout":"ok"}',
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
            workspace: workspaces.ref("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"),
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
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
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
          run: { id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31" },
        }),
        event.value,
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
      run: { id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31" },
    })
    expect(output.map((frame) => readEvent(frame).event.case)).toEqual([
      "entrypointReady",
      "taskChildInvokeRequested",
      "actorOutcome",
    ])
  })

  test("task.call unwrap throws the recorded remote Run failure", async () => {
    const child = task({ id: "resize-image", run: () => null })
    let failure: unknown
    const definition = task({
      id: "deploy",
      async run() {
        try {
          await child.call({
            workspace: workspaces.ref("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"),
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
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
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
          failure: {
            code: "task_failed",
            message: "resize failed",
            details: { stage: "decode" },
          },
          run: { id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc38" },
        }),
        event.value,
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
      name: "RunFailure",
      message: "resize failed",
      code: "task_failed",
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
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
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
          id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37",
          status: "pending",
          callback_url: "https://api.example.test/callback",
          public_access_token: "hlmr_pub_secret",
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
        token_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37",
      })
      yield actorDecision(
        waitEvent.value.correlationId,
        "completed",
        '{"approved":true}',
        waitEvent.value,
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

  test("does not expose generic retryability on Token Wait errors", async () => {
    let failure: unknown
    const definition = task({
      id: "deploy",
      async run() {
        const token = await tokens.create()
        const result = await token.wait()
        if (!result.ok) failure = result.error
        return null
      },
    })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []
    const createWritten = deferred<void>()
    const waitWritten = deferred<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      await createWritten.promise
      const createEvent = readEvent(output[1]!).event
      if (createEvent.case !== "tokenCreateRequested") return
      yield actorDecision(createEvent.value.correlationId, "completed", JSON.stringify({
        id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37",
        status: "pending",
        callback_url: "https://api.example.test/callback",
        public_access_token: "hlmr_pub_secret",
        timeout_at: "2026-07-24T12:00:00Z",
        metadata: {},
        tags: [],
        created_at: "2026-07-24T11:50:00Z",
        updated_at: "2026-07-24T11:50:00Z",
      }))
      await waitWritten.promise
      const waitEvent = readEvent(output[2]!).event
      if (waitEvent.case !== "runWaitRequested") return
      yield actorDecision(
        waitEvent.value.correlationId,
        "failed",
        JSON.stringify({ reason_code: "token_expired" }),
        waitEvent.value,
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
    expect(failure).toMatchObject({
      name: "HelmrError",
      code: "token_expired",
      message: "Token expired",
    })
    expect(Object.hasOwn(failure as object, "retryable")).toBe(false)
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
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))

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
          frameMessage(programProto.ProgramStartSchema, start),
          frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start)),
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
          await sessions.ref("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33").input.send(null, {
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
        frameMessage(programProto.ProgramStartSchema, start),
        frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start)),
      ),
      definition,
      output,
    }))
    expect(failure).toMatchObject({
      name: "HelmrError",
      code: "invalid_idempotency_key",
    })
    expect(Object.hasOwn(failure as object, "retryable")).toBe(false)
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
          await sessions.ref("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34").input.send({ hello: "world" })
        } catch (error) {
          failure = error
        }
      },
    })
    const start = actorStart(0n, 0n)
    const output: Uint8Array[] = []
    const sendWritten = deferred<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      await sendWritten.promise
      const send = readEvent(output[1]!).event
      if (send.case !== "sessionInputSendRequested") return
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
      message: "Actor does not accept new input",
    })
    expect(Object.hasOwn(failure as object, "retryable")).toBe(false)
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
            await sessions.ref("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33").input.send(
              null,
              {},
              { signal },
            )
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
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      await sendWritten.promise
      const send = readEvent(output[1]!).event
      if (send.case !== "sessionInputSendRequested") return
      yield actorDecision(send.value.correlationId, "completed", JSON.stringify({
        id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34",
        sequence: 3,
        data: null,
        source: {
          type: "run",
          run_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
        },
        created_at: "2026-07-26T00:00:00Z",
      }))
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
      "sessionInputSendRequested",
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
      input: gatedFrames(frameMessage(programProto.ProgramStartSchema, start), gate.promise, frameMessage(programProto.EntrypointReleaseSchema, release)),
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
        frameMessage(programProto.ProgramStartSchema, start),
        gate.promise,
        frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start)),
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
    let runWaitId = ""
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      await waitWritten.promise
      const event = readEvent(output[1]!).event
      expect(event.case).toBe("runWaitRequested")
      if (event.case !== "runWaitRequested") return
      correlationId = event.value.correlationId
      runWaitId = event.value.runWaitId
      expect(event.value.kind).toBe("timer")
      expect(event.value.timeoutMs).toBe(60_000n)
      expect(JSON.parse(event.value.paramsJson)).toEqual({ duration: "1m" })
      yield frameMessage(programProto.ResumeDecisionSchema, create(
        programProto.ResumeDecisionSchema,
        {
          runWaitId: event.value.runWaitId,
          correlationId: event.value.correlationId,
          kind: "completed",
          dataJson: "null",
          requireConsumedAck: true,
          checkpointId: "checkpoint-1",
          resumeAttachId: event.value.resumeAttachId,
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
      expect(consumed.value.runWaitId).toBe(runWaitId)
      expect(consumed.value.resumeRequestVersion).toBe(4n)
      expect(consumed.value.correlationId).toBe(correlationId)
    }

    const result = readEvent(output[3]!).event
    expect(result.case).toBe("taskOutcome")
    if (result.case === "taskOutcome" && result.value.outcome.case === "succeeded") {
      expect(result.value.outcome.value.outputJson).toBe('{"resumed":true}')
    }
  })

  test("rejects a durable Wait decision with mismatched allocated identity", async () => {
    const definition = task({
      id: "deploy",
      async run() {
        await timers.waitFor("1m")
      },
    })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []
    const waitWritten = deferred<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      await waitWritten.promise
      const event = readEvent(output[1]!).event
      if (event.case !== "runWaitRequested") return
      yield frameMessage(programProto.ResumeDecisionSchema, create(
        programProto.ResumeDecisionSchema,
        {
          correlationId: event.value.correlationId,
          runWaitId: event.value.runWaitId,
          resumeAttachId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc99",
          kind: "completed",
          dataJson: "null",
        },
      ))
    }

    await expect(runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => {
        if (output.length === 2) waitWritten.resolve()
      },
    }))).rejects.toThrow("did not match the pending Wait")
    expect(output).toHaveLength(2)
  })

  test("classifies a throwing payload schema as a bounded terminal validation failure", async () => {
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
        frameMessage(programProto.ProgramStartSchema, start),
        frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start)),
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
        frameMessage(programProto.ProgramStartSchema, start),
        frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start)),
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
        frameMessage(programProto.ProgramStartSchema, start),
        frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start)),
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

  test("canonicalizes Task handler failure messages", async () => {
    const definition = task({
      id: "deploy",
      run() {
        throw new Error("\u0085failed\u0085")
      },
    })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []

    await runProgram(locatorURL, programIO({
      input: frames(
        frameMessage(programProto.ProgramStartSchema, start),
        frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start)),
      ),
      definition,
      output,
    }))

    const result = readEvent(output[1]!).event
    expect(result.case).toBe("taskOutcome")
    if (result.case === "taskOutcome" && result.value.outcome.case === "failed") {
      expect(result.value.outcome.value.message).toBe("failed")
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
          frameMessage(programProto.ProgramStartSchema, start),
          frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start)),
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
        frameMessage(programProto.ProgramStartSchema, start),
        frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start)),
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
      frameMessage(programProto.ProgramStartSchema, start),
      frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start)),
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
        input: frames(frameMessage(programProto.ProgramStartSchema, taskStart("noPayload"))),
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
    const wrong = create(programProto.EntrypointReleaseSchema, {
      runId: start.runId,
      attemptNumber: start.attemptNumber + 1,
      entrypoint: taskIdentity("deploy"),
    })

    await expect(
      runProgram(locatorURL, programIO({
        input: frames(
          frameMessage(programProto.ProgramStartSchema, start),
          frameMessage(programProto.EntrypointReleaseSchema, wrong),
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
        architecture: "x86_64",
        configResultDigest: `sha256:${"4".repeat(64)}`,
        declarations: [
          {
            declaredId: "deploy",
            kind: "task",
            locator: {
              exportName: "definition",
              modulePath: `.helmr/modules/${"1".repeat(64)}.mjs`,
              slot: "handler",
            },
            manifest: {},
          },
          {
            declaredId: "worker",
            kind: "actor",
            locator: {
              exportName: "definition",
              modulePath: `tasks/.helmr/modules/${"2".repeat(64)}.mjs`,
              slot: "handler",
            },
            manifest: {},
          },
        ],
        formatVersion: 0,
        queues: [],
        runtimeContract: "helmr.runtime.v0",
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
): programProto.ProgramStart {
  return create(programProto.ProgramStartSchema, {
    entrypointDeclaredId: "deploy",
    runId: "run-1",
    attemptNumber: 1,
    cause: create(programProto.RunCauseSchema, {
      kind: {
        case: "api",
        value: create(programProto.ApiCauseSchema),
      },
    }),
    deploymentId: "deployment-1",
    deploymentVersion: "v1",
    workspaceId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc30",
    baseWorkspaceVersionId: "version-1",
    entrypoint: {
      case: "task",
      value: create(programProto.TaskStartSchema, {
        payload: payload === "noPayload"
          ? {
              case: "noPayload",
              value: create(programProto.NoPayloadSchema),
            }
          : {
              case: "payloadJson",
              value,
            },
      }),
    },
  })
}

function actorStart(start: bigint, highWatermark: bigint): programProto.ProgramStart {
  return create(programProto.ProgramStartSchema, {
    entrypointDeclaredId: "worker",
    runId: "run-1",
    attemptNumber: 1,
    cause: create(programProto.RunCauseSchema, {
      kind: { case: "actorStart", value: create(programProto.ActorStartCauseSchema) },
    }),
    deploymentId: "deployment-1",
    deploymentVersion: "v1",
    workspaceId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc30",
    baseWorkspaceVersionId: "version-1",
    entrypoint: {
      case: "actor",
      value: create(programProto.ActorStartSchema, {
        sessionId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
        startInputSequence: start,
        inputHighWatermark: highWatermark,
      }),
    },
  })
}

function releaseFor(start: programProto.ProgramStart): programProto.EntrypointRelease {
  return create(programProto.EntrypointReleaseSchema, {
    runId: start.runId,
    attemptNumber: start.attemptNumber,
    entrypoint: start.entrypoint.case === "actor"
      ? actorIdentity(start.entrypointDeclaredId)
      : taskIdentity(start.entrypointDeclaredId),
  })
}

function actorIdentity(declaredId: string): programProto.EntrypointIdentity {
  return create(programProto.EntrypointIdentitySchema, {
    declaredId,
    kind: { case: "actor", value: create(programProto.ActorEntrypointSchema) },
  })
}

function taskIdentity(declaredId: string): programProto.EntrypointIdentity {
  return create(programProto.EntrypointIdentitySchema, {
    declaredId,
    kind: {
      case: "task",
      value: create(programProto.TaskEntrypointSchema),
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
  wait?: { runWaitId: string; resumeAttachId: string },
): Uint8Array {
  return frameMessage(programProto.ResumeDecisionSchema, create(
    programProto.ResumeDecisionSchema,
    {
      correlationId,
      kind,
      dataJson,
      ...(wait === undefined
        ? {}
        : {
          runWaitId: wait.runWaitId,
          resumeAttachId: wait.resumeAttachId,
        }),
    },
  ))
}

function actorInput(sequence: number, value: unknown): string {
  return JSON.stringify({
    value,
    record: {
      id: `019c10d5-a6f7-7af1-8f5f-bb97bcc0d${sequence.toString(16).padStart(3, "0")}`,
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

function readEvent(value: Uint8Array): programProto.RunEvent {
  const length = new DataView(
    value.buffer,
    value.byteOffset,
    value.byteLength,
  ).getUint32(0)
  expect(length).toBe(value.byteLength - 4)
  return fromBinary(programProto.RunEventSchema, value.subarray(4))
}

function readConcatenatedEvents(value: Uint8Array): programProto.RunEvent[] {
  const result: programProto.RunEvent[] = []
  let offset = 0
  while (offset < value.byteLength) {
    if (value.byteLength - offset < 4) throw new Error("truncated event header")
    const size = new DataView(
      value.buffer,
      value.byteOffset + offset,
      4,
    ).getUint32(0)
    offset += 4
    if (value.byteLength - offset < size) throw new Error("truncated event body")
    result.push(fromBinary(
      programProto.RunEventSchema,
      value.subarray(offset, offset + size),
    ))
    offset += size
  }
  return result
}

async function* frames(...values: Uint8Array[]): AsyncIterable<Uint8Array> {
  for (const value of values) yield value
}

function observedFrames(
  values: readonly Uint8Array[],
  onReturn: () => Promise<void>,
): AsyncIterable<Uint8Array> {
  return {
    [Symbol.asyncIterator]() {
      let index = 0
      return {
        async next(): Promise<IteratorResult<Uint8Array>> {
          if (index >= values.length) return { done: true, value: undefined }
          return { done: false, value: values[index++]! }
        },
        async return(): Promise<IteratorResult<Uint8Array>> {
          await onReturn()
          return { done: true, value: undefined }
        },
      }
    },
  }
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
