import "./session-runtime.test"
import { create, fromBinary, toBinary } from "@bufbuild/protobuf"
import type { GenMessage } from "@bufbuild/protobuf/codegenv2"
import { programProto } from "@helmr/proto"
import { spawn } from "node:child_process"
import {
  image,
  logger,
  metadata,
  task,
  timers,
  tokens,
  sandbox,
  workspaces,
} from "@helmr/sdk"
import assert from "node:assert/strict"
import { describe, test } from "node:test"

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

    assert.deepEqual(lifecycle, [
      "entrypointReady",
      "taskOutcome",
      "input-closed",
    ])
  })

  test("releases input after handled Task failures", async () => {
    const cases = [
      {
        definition: task({ id: "deploy", run() { throw new Error("task failed") } }),
        start: taskStart("noPayload"),
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
      assert.equal(closeCount, 1)
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

    await assert.rejects(runProgram(locatorURL, programIO({
      input,
      definition,
      output,
    })), { message: /input release failed/ })
    assert.deepEqual(output.map((value) => readEvent(value).event.case), [
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

    await assert.rejects(runProgram(locatorURL, programIO({
      input,
      definition: task({ id: "other", run: () => null }),
      output: [],
    })), { message: /does not match/ })
    assert.equal(closeCount, 0)
  })

  test("Runtime input owner exits while its parent retains stdin", async () => {
    const start = taskStart("noPayload")
    const runtimeEntryURL = new URL(
      "./program.mjs",
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
              sourcePath: "tasks/main.ts",
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
    child.stdin.write(Buffer.concat([
      frameMessage(programProto.ProgramStartSchema, start),
      frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start)),
    ]))

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
      assert.deepEqual({ result, stderr }, {
        result: { code: 0, signal: null },
        stderr: "",
      })
    } finally {
      if (timeout !== undefined) clearTimeout(timeout)
      child.stdin.destroy()
      if (child.exitCode === null && child.signalCode === null) child.kill()
      await closed.catch(() => {})
    }
    assert.deepEqual(readConcatenatedEvents(Buffer.concat(output)).map((event) =>
      event.event.case
    ), ["entrypointReady", "taskOutcome"])
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
        const run = await child.start(
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
        return { id: run.id }
      },
    })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []
    const requested = Promise.withResolvers<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      await requested.promise
      const event = readEvent(output[1]!).event
      assert.equal(event.case, "taskChildInvokeRequested")
      if (event.case !== "taskChildInvokeRequested") return
      assert.equal(event.value.declaredId, "resize-image")
      assert.equal(event.value.method, "start")
      assert.equal(event.value.payloadPresent, true)
      assert.equal(event.value.payloadJson, '{"imageId":"image-1"}')
      assert.equal(event.value.workspaceJson, '{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"}')
      assert.deepEqual(JSON.parse(event.value.optionsJson), {
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
      assert.equal(event.value.idempotencyKey, "resize:image-1")
      yield runtimeDecision(
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
    assert.deepEqual(output.map((frame) => readEvent(frame).event.case), [
      "entrypointReady",
      "taskChildInvokeRequested",
      "taskOutcome",
    ])
    const outcome = readEvent(output[2]!).event
    assert.equal(outcome.case, "taskOutcome")
    if (outcome.case === "taskOutcome") {
      assert.equal(outcome.value.outcome.case, "succeeded")
      if (outcome.value.outcome.case === "succeeded") {
        assert.equal(outcome.value.outcome.value.outputJson, '{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"}')
      }
    }
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
          secrets: [{ secret: "TOKEN", env: { name: "TOKEN", mode: "raw" } }],
          idempotencyKey: "create:cache",
        })
        const workspace = await created.retrieve()
        const executed = await created.exec({
          command: ["sh", "-c", "printf ok"],
          stdin: new Uint8Array([1, 2, 3]),
          timeout: "1s",
          idempotencyKey: "exec:cache",
        })
        const deleted = await created.delete({ idempotencyKey: "delete:cache" })
        return {
          id: workspace.id,
          exitCode: executed.exitCode,
          stdout: new TextDecoder().decode(executed.stdout),
          deleted: deleted.workspaceId,
        }
      },
    })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []
    const requested = Array.from({ length: 4 }, () => Promise.withResolvers<void>())
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      const responses = [
        '{"workspace_id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"}',
        '{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32","key":"build-cache","sandbox_id":"cache","deployment_id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35","status":"available","secrets":[{"secret":"TOKEN","env":{"name":"TOKEN","mode":"raw"}}],"last_activity_at":"2026-07-26T00:00:00Z","created_at":"2026-07-26T00:00:00Z","updated_at":"2026-07-26T00:00:00Z"}',
        '{"exit_code":0,"stdout_base64":"b2s=","stderr_base64":""}',
        '{"workspace_id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"}',
      ]
      for (let index = 0; index < responses.length; index++) {
        await requested[index]!.promise
        const event = readEvent(output[index + 1]!).event
        if (event.case === undefined) return
        observed.push(event.case)
        if (event.case === "workspaceCreateRequested") {
          assert.equal(event.value.secrets.length, 1)
          assert.partialDeepStrictEqual(event.value.secrets[0], {
            secret: "TOKEN",
            placement: { case: "env", value: {name:"TOKEN",mode:"raw"} },
          })
        }
        const correlationId = "correlationId" in event.value
          ? event.value.correlationId as string
          : ""
        yield runtimeDecision(correlationId, "completed", responses[index]!)
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
    assert.deepEqual(observed, [
      "workspaceCreateRequested",
      "workspaceRetrieveRequested",
      "workspaceExecRequested",
      "workspaceDeleteRequested",
    ])
    const outcome = readEvent(output.at(-1)!).event
    assert.equal(outcome.case, "taskOutcome")
    if (outcome.case === "taskOutcome" &&
      outcome.value.outcome.case === "succeeded") {
      assert.equal(outcome.value.outcome.value.outputJson, '{"deleted":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32","exitCode":0,"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32","stdout":"ok"}')
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
    const definition = task({
      id: "deploy",
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
        return null
      },
    })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []
    const requested = Promise.withResolvers<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      await requested.promise
      const event = readEvent(output[1]!).event
      assert.equal(event.case, "taskChildInvokeRequested")
      if (event.case !== "taskChildInvokeRequested") return
      assert.equal(event.value.method, "call")
      assert.equal(event.value.actorSpeculativeInputSequence, undefined)
      assert.equal(event.value.idempotencyKey, "resize:image-1")
      yield runtimeDecision(
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
    assert.equal(overlappingWaitError, "only one consuming Wait may be pending")
    assert.deepEqual(result, {
      ok: true,
      output: { resized: true },
      run: { id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31" },
    })
    assert.deepEqual(output.map((frame) => readEvent(frame).event.case), [
      "entrypointReady",
      "taskChildInvokeRequested",
      "taskOutcome",
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
    const requested = Promise.withResolvers<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      await requested.promise
      const event = readEvent(output[1]!).event
      if (event.case !== "taskChildInvokeRequested") return
      assert.equal(event.value.method, "call")
      assert.equal(event.value.actorSpeculativeInputSequence, undefined)
      yield runtimeDecision(
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
    assert.ok(failure instanceof Error)
    assert.equal(failure.name, "RunFailure")
    assert.equal(failure.message, "resize failed")
    assert.ok("code" in failure)
    assert.equal(failure.code, "task_failed")
    assert.ok("details" in failure)
    assert.partialDeepStrictEqual(failure.details, { stage: "decode" })
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
    const createWritten = Promise.withResolvers<void>()
    const waitWritten = Promise.withResolvers<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      await createWritten.promise
      const createEvent = readEvent(output[1]!).event
      assert.equal(createEvent.case, "tokenCreateRequested")
      if (createEvent.case !== "tokenCreateRequested") return
      assert.equal(createEvent.value.timeoutMs, 600_000n)
      assert.equal(createEvent.value.idempotencyKey, "approval-1")
      assert.equal(createEvent.value.metadataJson, '{"approval":true}')
      assert.deepEqual(createEvent.value.tags, ["review"])
      yield runtimeDecision(
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
      assert.equal(waitEvent.case, "runWaitRequested")
      if (waitEvent.case !== "runWaitRequested") return
      assert.equal(waitEvent.value.kind, "token")
      assert.equal(waitEvent.value.timeoutMs, 1_800_000n)
      assert.equal(waitEvent.value.idleTimeoutMs, 45_000n)
      assert.equal(waitEvent.value.metadataJson, '{"stage":"approval"}')
      assert.deepEqual(waitEvent.value.tags, ["human"])
      assert.deepEqual(JSON.parse(waitEvent.value.paramsJson), {
        token_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37",
      })
      yield runtimeDecision(
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
    assert.deepEqual(completed, { approved: true })
    assert.deepEqual(output.map((frame) => readEvent(frame).event.case), [
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
    const createWritten = Promise.withResolvers<void>()
    const waitWritten = Promise.withResolvers<void>()
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      await createWritten.promise
      const createEvent = readEvent(output[1]!).event
      if (createEvent.case !== "tokenCreateRequested") return
      yield runtimeDecision(createEvent.value.correlationId, "completed", JSON.stringify({
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
      yield runtimeDecision(
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
    assert.ok(failure instanceof Error)
    assert.equal(failure.name, "HelmrError")
    assert.ok("code" in failure)
    assert.equal(failure.code, "token_expired")
    assert.equal(failure.message, "Token expired")
    assert.equal(Object.hasOwn(failure as object, "retryable"), false)
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
    const writes = [Promise.withResolvers<void>(), Promise.withResolvers<void>(), Promise.withResolvers<void>()]
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))

      await writes[0]!.promise
      const set = readEvent(output[1]!).event
      assert.equal(set.case, "metadataUpdated")
      if (set.case !== "metadataUpdated") return
      assert.partialDeepStrictEqual(set.value, {
        operation: "set",
        key: "phase",
        valueJson: '"running"',
      })
      yield runtimeDecision(set.value.correlationId, "completed", "{}")

      await writes[1]!.promise
      const increment = readEvent(output[2]!).event
      assert.equal(increment.case, "metadataUpdated")
      if (increment.case !== "metadataUpdated") return
      assert.partialDeepStrictEqual(increment.value, {
        operation: "increment",
        key: "steps",
        amount: 2,
      })
      yield runtimeDecision(increment.value.correlationId, "completed", "{}")

      await writes[2]!.promise
      const log = readEvent(output[3]!).event
      assert.equal(log.case, "structuredLogRequested")
      if (log.case !== "structuredLogRequested") return
      assert.partialDeepStrictEqual(log.value, {
        level: "error",
        message: "step failed",
      })
      assert.deepEqual(JSON.parse(log.value.attributesJson), {
        retryable: false,
        step: 2,
      })
      yield runtimeDecision(log.value.correlationId, "completed", "{}")
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
    assert.equal(readEvent(output[4]!).event.case, "taskOutcome")
  })

  for (const timeout of ["0.5s", "01s", " 1s"]) {
    test(`rejects non-canonical Token duration ${timeout} before emission`, async () => {
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
      assert.ok(caught instanceof Error)
      assert.ok(((caught as Error).message).includes("positive integer"))
      assert.deepEqual(output.map((frame) => readEvent(frame).event.case), [
        "entrypointReady",
        "taskOutcome",
      ])
    })
  }

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
    const gate = Promise.withResolvers<void>()
    const ready = Promise.withResolvers<void>()
    const output: Uint8Array[] = []
    const running = runProgram(locatorURL, programIO({
      input: gatedFrames(frameMessage(programProto.ProgramStartSchema, start), gate.promise, frameMessage(programProto.EntrypointReleaseSchema, release)),
      definition,
      output,
      onWrite: () => ready.resolve(),
    }))

    await ready.promise
    assert.equal(invoked, false)
    assert.equal(readEvent(output[0]!).event.case, "entrypointReady")

    gate.resolve()
    await running
    assert.equal(invoked, true)
    const result = readEvent(output[1]!).event
    assert.equal(result.case, "taskOutcome")
    if (result.case === "taskOutcome") {
      assert.equal(result.value.outcome.case, "succeeded")
      if (result.value.outcome.case === "succeeded") {
        assert.equal(result.value.outcome.value.outputJson, '{"runId":"run-1"}')
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
    const gate = Promise.withResolvers<void>()
    const ready = Promise.withResolvers<void>()
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
    assert.equal(validated, false)
    assert.equal(received, undefined)
    gate.resolve()
    await running
    assert.equal(validated, true)
    assert.equal(received, null)
    const result = readEvent(output[1]!).event
    assert.equal(result.case, "taskOutcome")
    if (result.case === "taskOutcome") {
      assert.equal(result.value.outcome.case, "succeeded")
      if (result.value.outcome.case === "succeeded") {
        assert.equal(result.value.outcome.value.outputJson, "null")
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
    const waitWritten = Promise.withResolvers<void>()
    let correlationId = ""
    let runWaitId = ""
    async function* input(): AsyncIterable<Uint8Array> {
      yield frameMessage(programProto.ProgramStartSchema, start)
      yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
      await waitWritten.promise
      const event = readEvent(output[1]!).event
      assert.equal(event.case, "runWaitRequested")
      if (event.case !== "runWaitRequested") return
      correlationId = event.value.correlationId
      runWaitId = event.value.runWaitId
      const allocatedIds = [correlationId, runWaitId, event.value.resumeAttachId]
      for (const id of allocatedIds) {
        assert.match(id, /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
      }
      assert.equal(new Set(allocatedIds).size, allocatedIds.length)
      assert.equal(event.value.kind, "timer")
      assert.equal(event.value.timeoutMs, 60_000n)
      assert.deepEqual(JSON.parse(event.value.paramsJson), { duration: "1m" })
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
    assert.equal(consumed.case, "resumeConsumed")
    if (consumed.case === "resumeConsumed") {
      assert.equal(consumed.value.runWaitId, runWaitId)
      assert.equal(consumed.value.resumeRequestVersion, 4n)
      assert.equal(consumed.value.correlationId, correlationId)
    }

    const result = readEvent(output[3]!).event
    assert.equal(result.case, "taskOutcome")
    if (result.case === "taskOutcome" && result.value.outcome.case === "succeeded") {
      assert.equal(result.value.outcome.value.outputJson, '{"resumed":true}')
    }
  })

  test("rejects a durable Wait decision with mismatched allocated identity", async () => {
    const definition = task({
      id: "deploy",
      async run() {
        await timers.waitFor("1m")
        return null
      },
    })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []
    const waitWritten = Promise.withResolvers<void>()
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

    await assert.rejects(runProgram(locatorURL, programIO({
      input: input(),
      definition,
      output,
      onWrite: () => {
        if (output.length === 2) waitWritten.resolve()
      },
    })), { message: /did not match the pending Wait/ })
    assert.equal(output.length, 2)
  })

  test("rejects malformed terminal Wait failure payloads as runtime protocol faults", async () => {
    const malformed = [
      "{",
      "null",
      "{}",
      JSON.stringify({ reason_code: "" }),
      JSON.stringify({ reason_code: 1 }),
    ]
    for (const dataJson of malformed) {
      const definition = task({
        id: "deploy",
        async run() {
          await timers.waitFor("1m")
          return null
        },
      })
      const start = taskStart("noPayload")
      const output: Uint8Array[] = []
      const waitWritten = Promise.withResolvers<void>()
      async function* input(): AsyncIterable<Uint8Array> {
        yield frameMessage(programProto.ProgramStartSchema, start)
        yield frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start))
        await waitWritten.promise
        const event = readEvent(output[1]!).event
        if (event.case !== "runWaitRequested") return
        yield runtimeDecision(
          event.value.correlationId,
          "failed",
          dataJson,
          event.value,
        )
      }
      await assert.rejects(runProgram(locatorURL, programIO({
        input: input(),
        definition,
        output,
        onWrite: () => {
          if (output.length === 2) waitWritten.resolve()
        },
      })), (error: unknown) => {
        assert.ok(error instanceof Error)
        assert.equal(error.name, "RuntimeProtocolError")
        assert.ok(error.cause instanceof Error)
        return true
      })
      assert.equal(output.length, 2)
      assert.equal(readEvent(output[1]!).event.case, "runWaitRequested")
    }
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

    assert.equal(invoked, false)
    const result = readEvent(output[1]!).event
    assert.equal(result.case, "taskOutcome")
    if (result.case === "taskOutcome") {
      assert.equal(result.value.outcome.case, "payloadInvalid")
      if (result.value.outcome.case === "payloadInvalid") {
        assert.equal(result.value.outcome.value.message, "task payload failed validation")
        const details = JSON.parse(result.value.outcome.value.detailsJson!)
        assert.ok(details.message.endsWith("…"))
        assert.ok(new TextEncoder().encode(details.message).byteLength <= 2_048)
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

    await assert.rejects(runProgram(locatorURL, programIO({
      input: frames(
        frameMessage(programProto.ProgramStartSchema, start),
        frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start)),
      ),
      definition,
      output,
      failWriteAt: 2,
    })), { message: /closed control stream/ })
    assert.equal(output.length, 1)
  })

  test("reports handler failures without granting retry authority", async () => {
    const exact = `${"猫".repeat(341)}a`
    const over = "猫".repeat(1_000)
    assert.equal(new TextEncoder().encode(exact).byteLength, 1_024)
    assert.ok(over.length < 1_024)
    assert.ok(new TextEncoder().encode(over).byteLength > 1_024)

    for (const { message, truncated } of [
      { message: exact, truncated: false },
      { message: over, truncated: true },
    ]) {
      const definition = task({
        id: "deploy",
        run() {
          throw new Error(message)
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
      assert.equal(result.case, "taskOutcome")
      if (result.case !== "taskOutcome") continue
      assert.equal(result.value.outcome.case, "failed")
      if (result.value.outcome.case !== "failed") continue
      const reported = result.value.outcome.value.message
      assert.ok(new TextEncoder().encode(reported).byteLength <= 1_024)
      if (truncated) {
        assert.ok(reported.endsWith("…"))
        assert.equal(result.value.outcome.value.detailsJson, undefined)
      } else {
        assert.equal(reported, message)
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
    assert.equal(result.case, "taskOutcome")
    if (result.case === "taskOutcome" && result.value.outcome.case === "failed") {
      assert.equal(result.value.outcome.value.message, "failed")
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
      assert.equal(result.case, "taskOutcome")
      if (result.case === "taskOutcome") {
        assert.equal(result.value.outcome.case, "failed")
      }
    }
  })

  test("does not translate an outcome transport failure into another outcome", async () => {
    const definition = task({ id: "deploy", run: () => null })
    const start = taskStart("noPayload")
    const output: Uint8Array[] = []

    await assert.rejects(runProgram(locatorURL, programIO({
      input: frames(
        frameMessage(programProto.ProgramStartSchema, start),
        frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start)),
      ),
      definition,
      output,
      failWriteAt: 2,
    })), { message: /closed control stream/ })
    assert.equal(output.length, 1)
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
    const input = Buffer.concat([
      frameMessage(programProto.ProgramStartSchema, start),
      frameMessage(programProto.EntrypointReleaseSchema, releaseFor(start)),
    ])

    await runProgram(locatorURL, programIO({
      input: byteFrames(input),
      definition,
      output: [],
    }))

    assert.equal(invoked, true)
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

    await assert.rejects(runProgram(locatorURL, programIO({
        input: frames(frameMessage(programProto.ProgramStartSchema, taskStart("noPayload"))),
        definition,
        output,
      })), { message: /does not match/ })
    assert.equal(output.length, 0)
    assert.equal(invoked, false)
  })

  test("rejects malformed and oversized frames before declaration import", async () => {
    let imported = false
    const malformed = frame(new Uint8Array([0xff]))
    const oversized = new Uint8Array(4)
    new DataView(oversized.buffer).setUint32(0, 256 * 1024 * 1024 + 1)

    for (const input of [malformed, oversized]) {
      await assert.rejects(runProgram(locatorURL, {
          input: frames(input),
          write: async () => {},
          readLocator: async () => {
            throw new Error("locator must not be read")
          },
          importModule: async () => {
            imported = true
            return {}
          },
        }))
    }
    assert.equal(imported, false)
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

    await assert.rejects(runProgram(locatorURL, programIO({
        input: frames(
          frameMessage(programProto.ProgramStartSchema, start),
          frameMessage(programProto.EntrypointReleaseSchema, wrong),
        ),
        definition,
        output: [],
      })), { message: /does not match/ })
    assert.equal(invoked, false)
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
              sourcePath: `tasks/main.ts`,
              slot: "handler",
            },
            manifest: {},
          },
          {
            declaredId: "worker",
            kind: "actor",
            locator: {
              exportName: "definition",
              sourcePath: `tasks/other.ts`,
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
  assert.equal(length, value.byteLength - 4)
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

async function* gatedFrames(
  first: Uint8Array,
  gate: Promise<void>,
  second: Uint8Array,
): AsyncIterable<Uint8Array> {
  yield first
  await gate
  yield second
}

function runtimeDecision(
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
