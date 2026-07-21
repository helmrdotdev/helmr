import { create, fromBinary, toBinary, type GenMessage } from "@bufbuild/protobuf"
import { runProto } from "@helmr/proto"
import { task } from "@helmr/sdk"
import { describe, expect, test } from "bun:test"

import { runProgram, type ProgramIO } from "./program"

const locatorURL = new URL(
  "file:///opt/helmr/program/helmr/declarations.json",
)

describe("runProgram", () => {
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

function releaseFor(start: runProto.ProgramStart): runProto.EntrypointRelease {
  return create(runProto.EntrypointReleaseSchema, {
    runId: start.runId,
    attemptNumber: start.attemptNumber,
    entrypoint: taskIdentity(start.entrypointDeclaredId),
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
