import { create, fromBinary, toBinary } from "@bufbuild/protobuf"
import { runProto } from "@helmr/proto"
import {
  canonicalizeJsonValue,
  inspectDefinition,
  installRuntimeOperations,
  type InternalActorDefinition,
  type InternalTaskDefinition,
  type RuntimeOperations,
} from "@helmr/sdk/internal"
import type {
  ActorExecutionContext,
  ActorSelf,
  JsonValue,
  RunCause,
  TaskExecutionContext,
} from "@helmr/sdk"
import { createWriteStream, promises as fs } from "node:fs"
import { randomUUID } from "node:crypto"
import path from "node:path"
import { fileURLToPath, pathToFileURL } from "node:url"

const MAX_PROGRAM_FRAME_BYTES = 256 * 1024 * 1024
const MAX_TASK_OUTPUT_BYTES = 16 * 1024 * 1024
const MAX_TASK_ERROR_BYTES = 16 * 1024
const MAX_TASK_ERROR_MESSAGE_BYTES = 1024

type InputChunk = Uint8Array | string

export interface ProgramIO {
  readonly input: AsyncIterable<InputChunk>
  readonly write: (frame: Uint8Array) => Promise<void>
  readonly readLocator?: (url: URL) => Promise<string>
  readonly importModule?: (url: URL) => Promise<Record<string, unknown>>
}

interface LocatedDeclaration {
  readonly declaredId: string
  readonly exportName: string
  readonly kind: "task" | "actor" | "run_stream"
  readonly modulePath: string
}

interface DeclarationLocator {
  readonly declarations: readonly LocatedDeclaration[]
  readonly formatVersion: 0
}

class FrameReader {
  readonly #iterator: AsyncIterator<InputChunk>
  #chunk: Uint8Array<ArrayBufferLike> = new Uint8Array()
  #offset = 0

  constructor(input: AsyncIterable<InputChunk>) {
    this.#iterator = input[Symbol.asyncIterator]()
  }

  async read(maxBytes = MAX_PROGRAM_FRAME_BYTES): Promise<Uint8Array> {
    const header = await this.#readExact(4)
    const size = new DataView(
      header.buffer,
      header.byteOffset,
      header.byteLength,
    ).getUint32(0)
    if (size > maxBytes) {
      throw new Error(`runtime frame length ${size} exceeds max ${maxBytes}`)
    }
    return this.#readExact(size)
  }

  async #readExact(size: number): Promise<Uint8Array> {
    const result = new Uint8Array(size)
    let written = 0
    while (written < size) {
      if (this.#offset === this.#chunk.byteLength) {
        const next = await this.#iterator.next()
        if (next.done) {
          throw new Error(
            `runtime frame ended after ${written} of ${size} bytes`,
          )
        }
        this.#chunk =
          typeof next.value === "string"
            ? new TextEncoder().encode(next.value)
            : next.value
        this.#offset = 0
        if (this.#chunk.byteLength === 0) continue
      }
      const count = Math.min(
        size - written,
        this.#chunk.byteLength - this.#offset,
      )
      result.set(
        this.#chunk.subarray(this.#offset, this.#offset + count),
        written,
      )
      this.#offset += count
      written += count
    }
    return result
  }
}

export async function runProgram(
  locatorURL: URL,
  io = defaultProgramIO(),
): Promise<void> {
  const reader = new FrameReader(io.input)
  const start = fromBinary(runProto.ProgramStartSchema, await reader.read())
  validateProgramStart(start)

  const locator = await loadDeclarationLocator(locatorURL, io)
  const kind = start.entrypoint.case
  if (kind !== "task" && kind !== "actor") {
    throw new Error("Program-start entrypoint is required")
  }
  const located = locator.declarations.filter(
    (declaration) =>
      declaration.kind === kind &&
      declaration.declaredId === start.entrypointDeclaredId,
  )
  if (located.length !== 1) {
    throw new Error(
      `Program declaration ${kind}:${JSON.stringify(start.entrypointDeclaredId)} was not found exactly once`,
    )
  }
  const declaration = located[0]!
  const moduleURL = resolveModuleURL(locatorURL, declaration.modulePath)
  const imported = io.importModule === undefined
    ? await import(moduleURL.href)
    : await io.importModule(moduleURL)
  const definition = inspectDefinition(imported[declaration.exportName])
  if (
    definition === undefined ||
    definition.kind !== declaration.kind ||
    definition.id !== declaration.declaredId ||
    (definition.kind !== "task" && definition.kind !== "actor")
  ) {
    throw new Error(
      `Program export ${JSON.stringify(declaration.exportName)} does not match ${kind}:${JSON.stringify(start.entrypointDeclaredId)}`,
    )
  }
  validateEntrypointContract(start, definition)

  const identity = entrypointIdentity(kind, start.entrypointDeclaredId)
  await writeRunEvent(io, {
    case: "entrypointReady",
    value: create(runProto.EntrypointReadySchema, {
      runId: start.runId,
      attemptNumber: start.attemptNumber,
      entrypoint: identity,
    }),
  })

  const release = fromBinary(
    runProto.EntrypointReleaseSchema,
    await reader.read(),
  )
  validateEntrypointRelease(release, start, kind)

  if (definition.kind === "task") {
    await runTask(start, definition, io, reader)
    return
  }
  await runActor(start, definition, io, reader)
}

async function loadDeclarationLocator(
  url: URL,
  io: ProgramIO,
): Promise<DeclarationLocator> {
  const raw = io.readLocator === undefined
    ? await fs.readFile(url, "utf8")
    : await io.readLocator(url)
  const value: unknown = JSON.parse(raw)
  if (typeof value !== "object" || value === null) {
    throw new Error("declaration locator must be an object")
  }
  const record = value as Record<string, unknown>
  if (
    record["formatVersion"] !== 0 ||
    !Array.isArray(record["declarations"]) ||
    record["declarations"].length === 0
  ) {
    throw new Error("declaration locator has an invalid v0 shape")
  }
  const declarations = record["declarations"].map((entry, index) =>
    parseLocatedDeclaration(entry, index)
  )
  return { declarations, formatVersion: 0 }
}

function parseLocatedDeclaration(
  value: unknown,
  index: number,
): LocatedDeclaration {
  if (typeof value !== "object" || value === null) {
    throw new Error(`declaration locator entry ${index} must be an object`)
  }
  const record = value as Record<string, unknown>
  if (
    (record["kind"] !== "task" &&
      record["kind"] !== "actor" &&
      record["kind"] !== "run_stream") ||
    typeof record["declaredId"] !== "string" ||
    record["declaredId"] === "" ||
    typeof record["exportName"] !== "string" ||
    record["exportName"] === "" ||
    typeof record["modulePath"] !== "string"
  ) {
    throw new Error(`declaration locator entry ${index} is invalid`)
  }
  return {
    kind: record["kind"],
    declaredId: record["declaredId"],
    exportName: record["exportName"],
    modulePath: validateModulePath(record["modulePath"]),
  }
}

function validateModulePath(value: string): string {
  if (
    value === "" ||
    value.startsWith("/") ||
    value.includes("\\") ||
    path.posix.normalize(value) !== value ||
    value === "helmr" ||
    value.startsWith("helmr/") ||
    value.split("/").includes("node_modules")
  ) {
    throw new Error("declaration modulePath is outside first-party Program source")
  }
  return value
}

function resolveModuleURL(locatorURL: URL, modulePath: string): URL {
  const root = path.dirname(path.dirname(fileURLToPath(locatorURL)))
  const resolved = path.resolve(root, modulePath)
  const relative = path.relative(root, resolved)
  if (
    relative === "" ||
    relative === ".." ||
    relative.startsWith(`..${path.sep}`) ||
    path.isAbsolute(relative)
  ) {
    throw new Error("declaration modulePath escapes the Program root")
  }
  return pathToFileURL(resolved)
}

function validateProgramStart(start: runProto.ProgramStart): void {
  if (
    start.runId === "" ||
    start.attemptNumber === 0 ||
    start.entrypointDeclaredId === "" ||
    start.deploymentId === "" ||
    start.deploymentVersion === "" ||
    start.workspaceId === "" ||
    start.baseWorkspaceVersionId === "" ||
    start.cause === undefined ||
    start.cause.kind.case === undefined
  ) {
    throw new Error("Program-start frame is missing required logical fields")
  }
}

function validateEntrypointContract(
  start: runProto.ProgramStart,
  definition: InternalTaskDefinition | InternalActorDefinition,
): void {
  if (definition.kind === "actor") {
    if (
      start.entrypoint.case !== "actor" ||
      start.entrypoint.value.startInputSequence < 0n ||
      start.entrypoint.value.inputHighWatermark <
        start.entrypoint.value.startInputSequence
    ) {
      throw new Error("Program-start Actor cursor authority is invalid")
    }
    return
  }
  const payload = start.entrypoint.case === "task"
    ? start.entrypoint.value.payload.case
    : undefined
  if (
    (definition.hasPayload && payload !== "payloadJson") ||
    (!definition.hasPayload && payload !== "noPayload")
  ) {
    throw new Error(
      `Program-start payload presence does not match task ${JSON.stringify(definition.id)}`,
    )
  }
}

function entrypointIdentity(
  kind: "task" | "actor",
  declaredId: string,
): runProto.EntrypointIdentity {
  return create(runProto.EntrypointIdentitySchema, {
    declaredId,
    kind: kind === "task"
      ? {
          case: "task",
          value: create(runProto.TaskEntrypointSchema),
        }
      : {
          case: "actor",
          value: create(runProto.ActorEntrypointSchema),
        },
  })
}

function validateEntrypointRelease(
  release: runProto.EntrypointRelease,
  start: runProto.ProgramStart,
  kind: "task" | "actor",
): void {
  if (
    release.runId !== start.runId ||
    release.attemptNumber !== start.attemptNumber ||
    release.entrypoint?.declaredId !== start.entrypointDeclaredId ||
    release.entrypoint.kind.case !== kind
  ) {
    throw new Error("entrypoint release does not match Program-start identity")
  }
}

async function runTask(
  start: runProto.ProgramStart,
  definition: InternalTaskDefinition,
  io: ProgramIO,
  reader: FrameReader,
): Promise<void> {
  let payload: unknown
  if (definition.hasPayload) {
    let failureDetails: JsonValue | undefined
    try {
      if (start.entrypoint.case !== "task" ||
          start.entrypoint.value.payload.case !== "payloadJson") {
        throw new Error("task payload is missing")
      }
      payload = JSON.parse(
        new TextDecoder("utf-8", { fatal: true }).decode(
          start.entrypoint.value.payload.value,
        ),
      )
      const parsed = await definition.payloadSchema!["~standard"].validate(
        payload,
      )
      if ("issues" in parsed && parsed.issues !== undefined) {
        failureDetails = validationDetails(parsed.issues)
      } else {
        payload = parsed.value
      }
    } catch (error) {
      failureDetails = {
        message: boundedUtf8(errorMessage(error), 2_048),
      }
    }
    if (failureDetails !== undefined) {
      await writeTaskFailure(
        io,
        "payload_invalid",
        "task payload failed validation",
        failureDetails,
      )
      return
    }
  }

  const context = taskContext(start)
	const uninstallRuntime = installRuntimeOperations(
		programRuntimeOperations(start, io, reader),
	)
  let normalized: Uint8Array
  try {
    let output: unknown
    if (definition.hasPayload) {
      output = await definition.handler(payload, context)
    } else {
      output = await definition.handler(context)
    }
    normalized = canonicalizeJsonValue(output as JsonValue)
    if (normalized.byteLength > MAX_TASK_OUTPUT_BYTES) {
      throw new Error(
        `task output exceeds ${MAX_TASK_OUTPUT_BYTES} bytes`,
      )
    }
  } catch (error) {
		await writeTaskFailure(io, "failed", errorMessage(error))
		return
	} finally {
		uninstallRuntime()
	}
  await writeRunEvent(io, {
    case: "taskOutcome",
    value: create(runProto.TaskOutcomeSchema, {
      outcome: {
        case: "succeeded",
        value: create(runProto.TaskSucceededSchema, {
          outputJson: new TextDecoder().decode(normalized),
        }),
      },
    }),
  })
}

function programRuntimeOperations(
  start: runProto.ProgramStart,
  io: ProgramIO,
  reader: FrameReader,
): RuntimeOperations {
  let pending = false
  const wait = async (
    params: JsonValue,
    timeoutSeconds: number,
  ): Promise<void> => {
    if (pending) {
      throw new Error("only one consuming Wait may be pending")
    }
    pending = true
    const correlationId = randomUUID()
    try {
      await writeRunEvent(io, {
        case: "runWaitRequested",
        value: create(runProto.RunWaitRequestedSchema, {
          correlationId,
          kind: "timer",
          paramsJson: new TextDecoder().decode(canonicalizeJsonValue(params)),
          timeout: timeoutSeconds,
        }),
      })
      const decision = fromBinary(
        runProto.ResumeDecisionSchema,
        await reader.read(),
      )
      if (
        (decision.correlationId || decision.runWaitId) !== correlationId ||
        (decision.kind !== "completed" &&
          decision.kind !== "failed" &&
          decision.kind !== "cancelled")
      ) {
        throw new Error("timer resume decision did not match the pending Wait")
      }
		if (decision.requireConsumedAck) {
			await writeRunEvent(io, {
				case: "resumeConsumed",
				value: create(runProto.ResumeConsumedSchema, {
					runWaitId: decision.runWaitId,
					checkpointId: decision.checkpointId,
					resumeAttachId: decision.resumeAttachId,
					resumeRequestVersion: decision.resumeRequestVersion,
					runLeaseId: decision.runLeaseId,
					correlationId: decision.correlationId,
				}),
			})
		}
      if (decision.kind !== "completed") {
        const failure = resumeFailure(decision.dataJson)
        throw new Error(`timer Wait ${decision.kind}: ${failure.reasonCode}`)
      }
    } finally {
      pending = false
    }
  }
  return {
    waitFor(duration) {
      const timeoutSeconds = durationSeconds(duration)
      return wait({ duration }, timeoutSeconds)
    },
    waitUntil(date) {
      if (!(date instanceof Date) || Number.isNaN(date.getTime())) {
        return Promise.reject(new Error("timers.waitUntil() requires a valid Date"))
      }
      const remainingMs = date.getTime() - Date.now()
      if (remainingMs <= 0) return Promise.resolve()
      return wait(
        { date: date.toISOString() },
        boundedTimerSeconds(Math.ceil(remainingMs / 1000)),
      )
    },
  }
}

function resumeFailure(dataJson: string): { readonly reasonCode: string } {
  let value: unknown
  try {
    value = JSON.parse(dataJson)
  } catch {
    throw new Error("terminal Wait failure data must be valid JSON")
  }
  if (
    value === null ||
    typeof value !== "object" ||
    Array.isArray(value) ||
    typeof (value as { readonly reason_code?: unknown }).reason_code !== "string" ||
    (value as { readonly reason_code: string }).reason_code.trim() === ""
  ) {
    throw new Error("terminal Wait failure data must contain reason_code")
  }
  return { reasonCode: (value as { readonly reason_code: string }).reason_code }
}

function durationSeconds(duration: string): number {
  const match = /^(\d+(?:\.\d+)?)(ms|s|m|h|d)$/.exec(duration.trim())
  if (match === null) {
    throw new Error("timer duration must use ms, s, m, h, or d units")
  }
  const amount = Number(match[1])
  const unit = match[2]
  const multiplierMs = unit === "ms"
    ? 1
    : unit === "s"
    ? 1000
    : unit === "m"
    ? 60_000
    : unit === "h"
    ? 3_600_000
    : 86_400_000
  if (!Number.isFinite(amount) || amount <= 0) {
    throw new Error("timer duration must be positive")
  }
  const milliseconds = amount * multiplierMs
  const maxMilliseconds = 365 * 24 * 60 * 60 * 1000
  if (milliseconds < 1 || milliseconds > maxMilliseconds) {
    throw new Error("timer duration must be between 1ms and 365d")
  }
  return boundedTimerSeconds(Math.ceil(milliseconds / 1000))
}

function boundedTimerSeconds(seconds: number): number {
  const maxSeconds = 365 * 24 * 60 * 60
  if (!Number.isSafeInteger(seconds) || seconds < 1 || seconds > maxSeconds) {
    throw new Error("timer duration must be between 1ms and 365d")
  }
  return seconds
}

async function runActor(
  start: runProto.ProgramStart,
  definition: InternalActorDefinition,
  io: ProgramIO,
  reader: FrameReader,
): Promise<void> {
  if (start.entrypoint.case !== "actor") {
    throw new Error("Actor Program-start entrypoint is required")
  }
  const terminalInputSequence = start.entrypoint.value.startInputSequence
  const uninstallRuntime = installRuntimeOperations(
    programRuntimeOperations(start, io, reader),
  )
  try {
    await definition.handler(unavailableActorSelf(), actorContext(start))
  } catch (error) {
    if (error instanceof ActorChannelTransportUnavailableError) throw error
    await writeActorFailure(
      io,
      terminalInputSequence,
      errorMessage(error),
    )
    return
  } finally {
    uninstallRuntime()
  }
  await writeRunEvent(io, {
    case: "actorOutcome",
    value: create(runProto.ActorOutcomeSchema, {
      terminalInputSequence,
      outcome: {
        case: "succeeded",
        value: create(runProto.ActorSucceededSchema),
      },
    }),
  })
}

class ActorChannelTransportUnavailableError extends Error {
  constructor() {
    super("Actor channel transport is not available")
    this.name = "ActorChannelTransportUnavailableError"
  }
}

function unavailableActorSelf(): ActorSelf {
  const unavailable = (): never => {
    throw new ActorChannelTransportUnavailableError()
  }
  return Object.freeze({
    input: Object.freeze({ receive: unavailable }),
    output: Object.freeze({
      append: unavailable,
      pipe: unavailable,
      writer: unavailable,
    }),
  }) as ActorSelf
}

function actorContext(start: runProto.ProgramStart): ActorExecutionContext {
  if (start.entrypoint.case !== "actor") {
    throw new Error("Actor Program-start entrypoint is required")
  }
  return Object.freeze({
    ...taskContext(start),
    actor: Object.freeze({
      id: start.entrypoint.value.actorId,
      ...(start.entrypoint.value.key === undefined
        ? {}
        : { key: start.entrypoint.value.key }),
    }),
  }) as ActorExecutionContext
}

async function writeActorFailure(
  io: ProgramIO,
  terminalInputSequence: bigint,
  message: string,
): Promise<void> {
  const normalizedMessage = boundedUtf8(
    message === "" ? "actor failed" : message,
    MAX_TASK_ERROR_MESSAGE_BYTES,
  )
  await writeRunEvent(io, {
    case: "actorOutcome",
    value: create(runProto.ActorOutcomeSchema, {
      terminalInputSequence,
      outcome: {
        case: "failed",
        value: create(runProto.ActorFailedSchema, {
          message: normalizedMessage,
        }),
      },
    }),
  })
}

function taskContext(start: runProto.ProgramStart): TaskExecutionContext {
  return Object.freeze({
    signal: new AbortController().signal,
    run: Object.freeze({
      id: start.runId,
      attemptNumber: start.attemptNumber,
      cause: runCause(start.cause!),
    }),
    deployment: Object.freeze({
      id: start.deploymentId,
      version: start.deploymentVersion,
    }),
    workspace: Object.freeze({
      id: start.workspaceId,
      attemptBaseVersionId: start.baseWorkspaceVersionId,
    }),
  }) as TaskExecutionContext
}

function runCause(cause: runProto.RunCause): RunCause {
  switch (cause.kind.case) {
    case "api":
      return { type: "api" }
    case "manual":
      return { type: "manual" }
    case "child":
      return {
        type: "child",
        parentRunId: cause.kind.value.parentRunId,
      }
    case "schedule":
      return {
        type: "schedule",
        scheduleId: cause.kind.value.scheduleId,
        scheduledAt: new Date(
          Number(cause.kind.value.scheduledAtUnixMs),
        ),
        ...(cause.kind.value.previousScheduledAtUnixMs === undefined
          ? {}
          : {
              lastScheduledAt: new Date(
                Number(cause.kind.value.previousScheduledAtUnixMs),
              ),
            }),
        timezone: cause.kind.value.timezone,
      }
    case "actorStart":
      return { type: "actor-start" }
    case "continuation":
      return { type: "continuation" }
    default:
      throw new Error("Program-start cause is required")
  }
}

async function writeTaskFailure(
  io: ProgramIO,
  kind: "failed" | "payload_invalid",
  message: string,
  details?: JsonValue,
): Promise<void> {
  const normalizedMessage = boundedUtf8(
    message === "" ? "task failed" : message,
    MAX_TASK_ERROR_MESSAGE_BYTES,
  )
  let detailsJson: string | undefined
  if (details !== undefined) {
    detailsJson = new TextDecoder().decode(canonicalizeJsonValue(details))
    const errorBytes = canonicalizeJsonValue({
      message: normalizedMessage,
      details,
    }).byteLength
    if (errorBytes > MAX_TASK_ERROR_BYTES) detailsJson = undefined
  }
  await writeRunEvent(io, {
    case: "taskOutcome",
    value: create(runProto.TaskOutcomeSchema, {
      outcome: kind === "failed"
        ? {
            case: "failed",
            value: create(runProto.TaskFailedSchema, {
              message: normalizedMessage,
              ...(detailsJson === undefined ? {} : { detailsJson }),
            }),
          }
        : {
            case: "payloadInvalid",
            value: create(runProto.TaskPayloadInvalidSchema, {
              message: normalizedMessage,
              ...(detailsJson === undefined ? {} : { detailsJson }),
            }),
          },
    }),
  })
}

function validationDetails(
  issues: readonly {
    readonly message: string
    readonly path?: readonly (PropertyKey | { readonly key: PropertyKey })[]
  }[],
): JsonValue {
  return {
    issues: issues.slice(0, 5).map((issue) => ({
      message: boundedUtf8(issue.message, 1_024),
      ...(issue.path === undefined
        ? {}
        : {
            path: issue.path.slice(0, 16).map((part) =>
              boundedUtf8(
                String(
                  typeof part === "object" && part !== null && "key" in part
                    ? part.key
                    : part,
                ),
                256,
              )
            ),
          }),
    })),
    truncated: issues.length > 5,
  }
}

function boundedUtf8(value: string, maxBytes: number): string {
  const encoder = new TextEncoder()
  if (encoder.encode(value).byteLength <= maxBytes) return value
  const suffix = "…"
  const suffixBytes = encoder.encode(suffix).byteLength
  let result = ""
  let size = 0
  for (const character of value) {
    const characterBytes = encoder.encode(character).byteLength
    if (size + characterBytes + suffixBytes > maxBytes) break
    result += character
    size += characterBytes
  }
  return result + suffix
}

async function writeRunEvent(
  io: ProgramIO,
  event: runProto.RunEvent["event"],
): Promise<void> {
  const body = toBinary(
    runProto.RunEventSchema,
    create(runProto.RunEventSchema, { event }),
  )
  await io.write(frame(body))
}

function frame(body: Uint8Array): Uint8Array {
  if (body.byteLength > MAX_PROGRAM_FRAME_BYTES) {
    throw new Error(
      `runtime frame length ${body.byteLength} exceeds max ${MAX_PROGRAM_FRAME_BYTES}`,
    )
  }
  const result = new Uint8Array(4 + body.byteLength)
  new DataView(result.buffer).setUint32(0, body.byteLength)
  result.set(body, 4)
  return result
}

function defaultProgramIO(): ProgramIO {
  const output = createWriteStream("/dev/null", {
    fd: 3,
    autoClose: false,
  })
  return {
    input: process.stdin,
    write: (value) =>
      new Promise<void>((resolve, reject) => {
        output.write(value, (error) => {
          if (error === null || error === undefined) resolve()
          else reject(error)
        })
      }),
  }
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}
