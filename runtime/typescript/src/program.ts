import { create, fromBinary, toBinary } from "@bufbuild/protobuf"
import { runProto } from "@helmr/proto"
import {
  canonicalizeJsonValue,
  inspectDefinition,
  installRuntimeOperations,
  parseWorkspaceDeleteReceipt,
  parseWorkspaceExecResult,
  parseWorkspaceFileContent,
  parseWorkspaceFileEntry,
  parseWorkspaceFilePage,
  parseWorkspaceSnapshot,
  trimGoSpace,
  type InternalActorDefinition,
  type InternalTaskDefinition,
  type RuntimeOperations,
} from "@helmr/sdk/internal"
import type {
  ActorExecutionContext,
  ActorInputResult,
  ActorOperationOptions,
  ActorOperationReceipt,
  ActorOutputRecord,
  ActorReceive,
  ActorSelf,
  ActorStartOptions,
  ActorStatus,
  Duration,
  JsonValue,
  LogAttributes,
  Metadata,
  OutputAppendOptions,
  OutputSequenceOptions,
  ReceiveOptions,
  RunCause,
  RunLogLevel,
  RetryPolicy,
  SendOptions,
  Serializable,
  TaskCallOptions,
  TaskExecutionContext,
  TaskResult,
  TokenCreateOptions,
  TokenCreateResult,
  TokenWaitOptions,
  WorkspaceDeleteReceipt,
  WorkspaceDeleteRequest,
  WorkspaceExecRequest,
  WorkspaceExecResult,
  WorkspaceFileEntry,
  WorkspaceFileListQuery,
  WorkspaceSnapshot,
} from "@helmr/sdk"
import { createWriteStream, promises as fs } from "node:fs"
import { randomUUID } from "node:crypto"
import path from "node:path"
import { fileURLToPath, pathToFileURL } from "node:url"
import {
  analyzeProject,
  encodeVerificationResultFrame,
  failedVerificationResult,
  successfulVerificationResult,
} from "./analysis"

const MAX_PROGRAM_FRAME_BYTES = 256 * 1024 * 1024
const MAX_TASK_OUTPUT_BYTES = 16 * 1024 * 1024
const MAX_TASK_ERROR_BYTES = 16 * 1024
const MAX_RUN_LOG_MESSAGE_BYTES = 4 * 1024
const MAX_RUN_LOG_ATTRIBUTES_BYTES = 16 * 1024
const MAX_TASK_ERROR_MESSAGE_BYTES = 1024
const MAX_ACTOR_INPUT_BYTES = 1 * 1024 * 1024

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
  readonly kind: "task" | "actor"
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

class ResumeDecisionRouter {
  readonly #reader: FrameReader
  readonly #pending = new Map<string, {
    readonly resolve: (decision: runProto.ResumeDecision) => void
    readonly reject: (error: Error) => void
  }>()
  #reading = false

  constructor(reader: FrameReader) {
    this.#reader = reader
  }

  register(correlationId: string): Promise<runProto.ResumeDecision> {
    if (this.#pending.has(correlationId)) {
      return Promise.reject(new Error("duplicate runtime correlation id"))
    }
    const result = new Promise<runProto.ResumeDecision>((resolve, reject) => {
      this.#pending.set(correlationId, { resolve, reject })
    })
    this.#pump()
    return result
  }

  cancel(correlationId: string): void {
    this.#pending.delete(correlationId)
  }

  abandonPending(): void {
    this.#pending.clear()
  }

  #pump(): void {
    if (this.#reading) return
    this.#reading = true
    void (async () => {
      try {
        while (this.#pending.size > 0) {
          const decision = fromBinary(
            runProto.ResumeDecisionSchema,
            await this.#reader.read(),
          )
          const correlationId = decision.correlationId || decision.runWaitId
          const pending = this.#pending.get(correlationId)
          if (pending === undefined) {
            throw new Error("resume decision did not match a pending runtime operation")
          }
          this.#pending.delete(correlationId)
          pending.resolve(decision)
        }
      } catch (error) {
        const failure = error instanceof Error ? error : new Error(String(error))
        for (const pending of this.#pending.values()) pending.reject(failure)
        this.#pending.clear()
      } finally {
        this.#reading = false
        if (this.#pending.size > 0) this.#pump()
      }
    })()
  }
}

class RuntimeProtocolError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = "RuntimeProtocolError"
  }
}

class ActorCancellationError extends Error {
  readonly code: string

  constructor(reasonCode: string) {
    super(`Actor execution was cancelled: ${reasonCode}`)
    this.name = "AbortError"
    this.code = reasonCode
  }
}

class RunOperationState {
  readonly controller = new AbortController()
  #active = 0
  readonly #drainable = new Set<Promise<unknown>>()
  #protocolFault: RuntimeProtocolError | undefined

  track<T>(operation: () => Promise<T>): Promise<T> {
    this.#active++
    const result = (async () => {
      try {
        return await operation()
      } catch (error) {
        if (error instanceof RuntimeProtocolError && this.#protocolFault === undefined) {
          this.#protocolFault = error
        }
        throw error
      } finally {
        this.#active--
      }
    })()
    void result.catch(() => {})
    return result
  }

  trackDrainable<T>(operation: () => Promise<T>): Promise<T> {
    const result = this.track(operation)
    this.#drainable.add(result)
    void result.finally(() => {
      this.#drainable.delete(result)
    }).catch(() => {})
    return result
  }

  async drainForCompletion(): Promise<void> {
    while (this.#drainable.size !== 0) {
      await Promise.allSettled([...this.#drainable])
    }
  }

  cancel(reasonCode: string): ActorCancellationError {
    const error = new ActorCancellationError(reasonCode)
    if (!this.controller.signal.aborted) this.controller.abort(error)
    return this.controller.signal.reason as ActorCancellationError
  }

  assertCanComplete(): void {
    if (this.#protocolFault !== undefined) throw this.#protocolFault
    if (this.controller.signal.aborted) {
      throw this.controller.signal.reason as ActorCancellationError
    }
    if (this.#active !== 0) {
      throw new RuntimeProtocolError("Run handler returned with runtime operations still pending")
    }
  }
}

class ConsumingWaitGate {
  #pending = false

  acquire(error: () => Error = () => new Error("only one consuming Wait may be pending")): () => void {
    if (this.#pending) throw error()
    this.#pending = true
    let released = false
    return () => {
      if (released) return
      released = true
      this.#pending = false
    }
  }
}

async function requestRuntimeDecision(
  io: ProgramIO,
  decisions: ResumeDecisionRouter,
  correlationId: string,
  event: runProto.RunEvent["event"],
): Promise<runProto.ResumeDecision> {
  const pending = decisions.register(correlationId)
  try {
    await writeRunEvent(io, event)
  } catch (error) {
    decisions.cancel(correlationId)
    throw new RuntimeProtocolError("failed to write runtime operation request", {
      cause: error,
    })
  }
  try {
    return await pending
  } catch (error) {
    throw new RuntimeProtocolError("failed to read runtime operation decision", {
      cause: error,
    })
  }
}

async function writeRuntimeProtocolEvent(
  io: ProgramIO,
  event: runProto.RunEvent["event"],
): Promise<void> {
  try {
    await writeRunEvent(io, event)
  } catch (error) {
    throw new RuntimeProtocolError("failed to write runtime protocol event", {
      cause: error,
    })
  }
}

function parseRuntimeProtocolValue<T>(label: string, parse: () => T): T {
  try {
    return parse()
  } catch (error) {
    if (error instanceof RuntimeProtocolError) throw error
    throw new RuntimeProtocolError(`${label} was invalid`, { cause: error })
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
  const decisions = new ResumeDecisionRouter(reader)

  if (definition.kind === "task") {
    await runTask(start, definition, io, decisions)
    return
  }
  await runActor(start, definition, io, decisions)
}

export async function runVerification(
  root: string,
  architecture: "x86_64",
): Promise<void> {
  try {
    const result = await analyzeProject({ root, architecture })
    await writeSupervisorBytes(
      encodeVerificationResultFrame(successfulVerificationResult(result)),
    )
  } catch (error) {
    await writeSupervisorBytes(encodeVerificationResultFrame(
      failedVerificationResult(
        supervisorFailureMessage(error, "verification failed"),
      ),
    ))
  }
}

async function writeSupervisorBytes(value: Uint8Array): Promise<void> {
  const configured = process.env["HELMR_SUPERVISOR_FD"]
  const fd = configured === undefined ? 3 : Number(configured)
  if (!Number.isSafeInteger(fd) || fd < 3) {
    throw new Error("supervisor result descriptor is invalid")
  }
  const output = createWriteStream("", { fd, autoClose: false })
  await new Promise<void>((resolve, reject) => {
    output.once("error", reject)
    output.end(value, resolve)
  })
}

function supervisorFailureMessage(error: unknown, fallback: string): string {
  const message = error instanceof Error ? error.message : String(error)
  const normalized = message.trim() || fallback
  const bytes = Buffer.from(normalized)
  if (bytes.length <= 16 << 10) return normalized
  return bytes.subarray(0, 16 << 10).toString("utf8").replace(/\uFFFD+$/u, "")
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
    (record["kind"] !== "task" && record["kind"] !== "actor") ||
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
  decisions: ResumeDecisionRouter,
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
  const runOperations = new RunOperationState()
	const uninstallRuntime = installRuntimeOperations(
		programRuntimeOperations(start, io, decisions, new ConsumingWaitGate(), runOperations),
	)
  let normalized: Uint8Array
  try {
    let output: unknown
    if (definition.hasPayload) {
      output = await definition.handler(payload, context)
    } else {
      output = await definition.handler(context)
    }
    await runOperations.drainForCompletion()
    runOperations.assertCanComplete()
    normalized = canonicalizeJsonValue(output as JsonValue)
    if (normalized.byteLength > MAX_TASK_OUTPUT_BYTES) {
      throw new Error(
        `task output exceeds ${MAX_TASK_OUTPUT_BYTES} bytes`,
      )
    }
	} catch (error) {
		if (error instanceof RuntimeProtocolError) throw error
    await runOperations.drainForCompletion()
    runOperations.assertCanComplete()
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
  decisions: ResumeDecisionRouter,
  waitGate: ConsumingWaitGate,
  runOperations: RunOperationState,
  actorCursor?: { readonly value: bigint },
): RuntimeOperations {
  const performTaskStart = async (
    target: Readonly<{ declaredId: string; payloadPresent: boolean }>,
    payload: JsonValue | undefined,
    options: import("@helmr/sdk").TaskStartOptions,
  ): Promise<import("@helmr/sdk").RunHandle> => {
    if (options.signal?.aborted) throw abortSignalReason(options.signal)
    const idempotencyKey =
      options.idempotencyKey === "" ? undefined : options.idempotencyKey
    if (
      idempotencyKey === undefined &&
      process.env["NODE_ENV"] !== "production"
    ) {
      process.emitWarning(
        `Task "${target.declaredId}" was started without an idempotencyKey; retrying the parent Run may create another child Run.`,
        { code: "HELMR_KEYLESS_CHILD_TASK_START" },
      )
    }
    const correlationId = randomUUID()
    const payloadJson = target.payloadPresent
      ? new TextDecoder().decode(canonicalizeJsonValue(payload as JsonValue))
      : undefined
    const workspaceJson = new TextDecoder().decode(
      canonicalizeJsonValue(
        "id" in options.workspace
          ? { id: options.workspace.id }
          : { key: options.workspace.key },
      ),
    )
    const requestOptions = {
      ...(options.queue === undefined ? {} : { queue: options.queue }),
      ...(options.concurrencyKey === undefined
        ? {}
        : { concurrency_key: options.concurrencyKey }),
      ...(options.priority === undefined ? {} : { priority: options.priority }),
      ...(options.ttl === undefined ? {} : { ttl: options.ttl }),
      ...(options.retry === undefined
        ? {}
        : { retry: taskRetryRequest(options.retry) }),
      ...(options.metadata === undefined ? {} : { metadata: options.metadata }),
      ...(options.tags === undefined ? {} : { tags: [...options.tags] }),
    } satisfies JsonValue
    const optionsJson = new TextDecoder().decode(
      canonicalizeJsonValue(requestOptions),
    )
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "taskChildInvokeRequested",
        value: create(runProto.TaskChildInvokeRequestedSchema, {
          correlationId,
          declaredId: target.declaredId,
          method: "start",
          payloadPresent: target.payloadPresent,
          ...(payloadJson === undefined ? {} : { payloadJson }),
          workspaceJson,
          optionsJson,
          ...(idempotencyKey === undefined ? {} : { idempotencyKey }),
        }),
      })
      requireRuntimeOperationDecision(decision, correlationId, "Task child start")
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Task child start", decision.dataJson)
      }
      return parseRuntimeProtocolValue("Task child start result", () => {
        const value = JSON.parse(decision.dataJson) as unknown
        if (typeof value !== "object" || value === null || Array.isArray(value)) {
          throw new Error("result must be an object")
        }
        const keys = Object.keys(value)
        if (keys.length !== 1 || keys[0] !== "run_id") {
          throw new Error("result fields are invalid")
        }
        const id = (value as Record<string, unknown>)["run_id"]
        if (typeof id !== "string" || !/^run_[a-z2-7]{26}$/.test(id)) {
          throw new Error("run_id is invalid")
        }
        return Object.freeze({ id })
      })
    })
    return await abortableRuntimeOperation(operation, options.signal)
  }
  const performTaskCall = (
    target: Readonly<{ declaredId: string; payloadPresent: boolean }>,
    payload: JsonValue | undefined,
    options: TaskCallOptions,
  ): Promise<TaskResult<JsonValue>> => {
    if (options.signal?.aborted) {
      return Promise.reject(abortSignalReason(options.signal))
    }
    const operation = runOperations.track(async () => {
      const releaseWait = waitGate.acquire()
      try {
        const correlationId = randomUUID()
        const payloadJson = target.payloadPresent
          ? new TextDecoder().decode(
              canonicalizeJsonValue(payload as JsonValue),
            )
          : undefined
        const workspaceJson = new TextDecoder().decode(
          canonicalizeJsonValue(
            "id" in options.workspace
              ? { id: options.workspace.id }
              : { key: options.workspace.key },
          ),
        )
        const requestOptions = {
          ...(options.queue === undefined ? {} : { queue: options.queue }),
          ...(options.concurrencyKey === undefined
            ? {}
            : { concurrency_key: options.concurrencyKey }),
          ...(options.priority === undefined
            ? {}
            : { priority: options.priority }),
          ...(options.ttl === undefined ? {} : { ttl: options.ttl }),
          ...(options.retry === undefined
            ? {}
            : { retry: taskRetryRequest(options.retry) }),
          ...(options.metadata === undefined
            ? {}
            : { metadata: options.metadata }),
          ...(options.tags === undefined ? {} : { tags: [...options.tags] }),
        } satisfies JsonValue
        const decision = await requestRuntimeDecision(
          io,
          decisions,
          correlationId,
          {
            case: "taskChildInvokeRequested",
            value: create(runProto.TaskChildInvokeRequestedSchema, {
              correlationId,
              declaredId: target.declaredId,
              method: "call",
              payloadPresent: target.payloadPresent,
              ...(payloadJson === undefined ? {} : { payloadJson }),
              workspaceJson,
              optionsJson: new TextDecoder().decode(
                canonicalizeJsonValue(requestOptions),
              ),
              idempotencyKey: options.idempotencyKey,
              ...(actorCursor === undefined
                ? {}
                : { actorSpeculativeInputSequence: actorCursor.value }),
            }),
          },
        )
        if (
          (decision.correlationId || decision.runWaitId) !== correlationId ||
          (decision.kind !== "completed" &&
            decision.kind !== "failed" &&
            decision.kind !== "cancelled")
        ) {
          throw new RuntimeProtocolError(
            "Task child call decision did not match the pending Wait",
          )
        }
        if (decision.requireConsumedAck) {
          await writeRuntimeProtocolEvent(io, {
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
        if (decision.kind === "cancelled" && actorCursor !== undefined) {
          const failure = parseRuntimeProtocolValue(
            "Actor child Task cancellation decision",
            () => resumeFailure(decision.dataJson),
          )
          throw runOperations.cancel(failure.reasonCode)
        }
        if (decision.kind !== "completed") {
          throw decision.kind === "failed"
            ? runtimeOperationFailure("Task child call", decision.dataJson)
            : new RuntimeProtocolError(
                `Task child call was cancelled: ${
                  resumeFailure(decision.dataJson).reasonCode
                }`,
              )
        }
        return parseRuntimeProtocolValue(
          "Task child call result",
          () => parseTaskResult(decision.dataJson),
        )
      } finally {
        releaseWait()
      }
    })
    return abortableRuntimeOperation(operation, options.signal)
  }
  const performWait = async (
    params: JsonValue,
    timeoutMs: number,
  ): Promise<void> => {
    const releaseWait = waitGate.acquire()
    const correlationId = randomUUID()
    try {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "runWaitRequested",
        value: create(runProto.RunWaitRequestedSchema, {
          correlationId,
          kind: "timer",
          paramsJson: new TextDecoder().decode(canonicalizeJsonValue(params)),
          timeoutMs: BigInt(timeoutMs),
          ...(actorCursor === undefined
            ? {}
            : { actorSpeculativeInputSequence: actorCursor.value }),
        }),
      })
      if (
        (decision.correlationId || decision.runWaitId) !== correlationId ||
        (decision.kind !== "completed" &&
          decision.kind !== "failed" &&
          decision.kind !== "cancelled")
      ) {
        throw new RuntimeProtocolError("timer resume decision did not match the pending Wait")
      }
      if (decision.requireConsumedAck) {
			await writeRuntimeProtocolEvent(io, {
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
      if (decision.kind === "cancelled" && actorCursor !== undefined) {
        const failure = parseRuntimeProtocolValue(
          "Actor timer cancellation decision",
          () => resumeFailure(decision.dataJson),
        )
        throw runOperations.cancel(failure.reasonCode)
      }
      if (decision.kind !== "completed") {
        const failure = parseRuntimeProtocolValue(
          "timer Wait failure decision",
          () => resumeFailure(decision.dataJson),
        )
        throw new RuntimeProtocolError(`timer Wait ${decision.kind}: ${failure.reasonCode}`)
      }
    } finally {
      releaseWait()
    }
  }
  const wait = (params: JsonValue, timeoutMs: number): Promise<void> =>
    runOperations.track(() => performWait(params, timeoutMs))
  const performActorInputSend = async (
    target: Readonly<{
      declaredId: string
      address: { readonly id: string } | { readonly key: string }
    }>,
    input: JsonValue,
    options?: SendOptions,
  ): Promise<{ sequence: number }> => {
    if (options?.signal?.aborted) {
      throw abortSignalReason(options.signal)
    }
    const idempotencyKey = normalizeActorInputIdempotencyKey(
      options?.idempotencyKey,
    )
    const normalized = canonicalizeJsonValue(input)
    if (normalized.byteLength > MAX_ACTOR_INPUT_BYTES) {
      throw actorInputSendError(
        "actor_input_too_large",
        `Actor input exceeds ${MAX_ACTOR_INPUT_BYTES} bytes`,
        false,
      )
    }
    const correlationId = randomUUID()
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "actorInputSendRequested",
        value: create(runProto.ActorInputSendRequestedSchema, {
          correlationId,
          declaredId: target.declaredId,
          address: "id" in target.address
            ? { case: "actorId", value: target.address.id }
            : { case: "actorKey", value: target.address.key },
          dataJson: new TextDecoder().decode(normalized),
          ...(idempotencyKey === undefined
            ? {}
            : { idempotencyKey }),
        }),
      })
      requireRuntimeOperationDecision(
        decision,
        correlationId,
        "Actor input send",
      )
      if (decision.kind === "failed") {
        throw parseRuntimeProtocolValue(
          "Actor input send failure",
          () => parseActorInputSendFailure(decision.dataJson),
        )
      }
      return parseRuntimeProtocolValue(
        "Actor input send result",
        () => parseActorInputSendResult(decision.dataJson),
      )
    })
    return await abortableRuntimeOperation(operation, options?.signal)
  }
  const actorAddress = (
    address: { readonly id: string } | { readonly key: string },
  ) => "id" in address
    ? { case: "actorId" as const, value: address.id }
    : { case: "actorKey" as const, value: address.key }
  const performActorStart = async (
    declaredId: string,
    options: ActorStartOptions,
  ): Promise<Readonly<{ actorId: string; runId: string }>> => {
    if (options.signal?.aborted) throw abortSignalReason(options.signal)
    const correlationId = randomUUID()
    const run = options.run
    const runOptions = {
      ...(run?.queue === undefined ? {} : { queue: run.queue }),
      ...(run?.concurrencyKey === undefined
        ? {}
        : { concurrency_key: run.concurrencyKey }),
      ...(run?.priority === undefined ? {} : { priority: run.priority }),
      ...(run?.ttl === undefined ? {} : { ttl: run.ttl }),
      ...(run?.retry === undefined
        ? {}
        : { retry: taskRetryRequest(run.retry) }),
      ...(run?.metadata === undefined ? {} : { metadata: run.metadata }),
      ...(run?.tags === undefined ? {} : { tags: [...run.tags] }),
    } satisfies JsonValue
    const inputPresent = Object.hasOwn(options, "input")
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "actorStartRequested",
        value: create(runProto.ActorStartRequestedSchema, {
          correlationId,
          declaredId,
          workspace: "id" in options.workspace
            ? { case: "workspaceId", value: options.workspace.id }
            : { case: "workspaceKey", value: options.workspace.key },
          ...(options.key === undefined ? {} : { key: options.key }),
          ...(inputPresent
            ? {
                inputJson: new TextDecoder().decode(
                  canonicalizeJsonValue(options.input as JsonValue),
                ),
              }
            : {}),
          ...(options.idempotencyKey === undefined
            ? {}
            : { idempotencyKey: options.idempotencyKey }),
          runOptionsJson: new TextDecoder().decode(
            canonicalizeJsonValue(runOptions),
          ),
        }),
      })
      requireRuntimeOperationDecision(decision, correlationId, "Actor start")
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Actor start", decision.dataJson)
      }
      return parseRuntimeProtocolValue("Actor start result", () => {
        const value = parseObjectJSON(decision.dataJson, "Actor start result")
        requireExactKeys(value, ["actor_id", "run_id"], "Actor start result")
        const actorId = stringField(value, "actor_id", "Actor start result")
        const runId = stringField(value, "run_id", "Actor start result")
        if (!/^act_[a-z2-7]{26}$/.test(actorId) ||
          !/^run_[a-z2-7]{26}$/.test(runId)) {
          throw new Error("Actor start result has invalid public IDs")
        }
        return Object.freeze({ actorId, runId })
      })
    })
    return abortableRuntimeOperation(operation, options.signal)
  }
  const performActorStatus = async (
    target: Readonly<{
      declaredId: string
      address: { readonly id: string } | { readonly key: string }
    }>,
  ): Promise<ActorStatus> => {
    const correlationId = randomUUID()
    const decision = await requestRuntimeDecision(io, decisions, correlationId, {
      case: "actorStatusRequested",
      value: create(runProto.ActorStatusRequestedSchema, {
        correlationId,
        declaredId: target.declaredId,
        address: actorAddress(target.address),
      }),
    })
    requireRuntimeOperationDecision(decision, correlationId, "Actor status")
    if (decision.kind === "failed") {
      throw runtimeOperationFailure("Actor status", decision.dataJson)
    }
    return parseRuntimeProtocolValue(
      "Actor status result",
      () => parseActorStatus(decision.dataJson),
    )
  }
  const performActorClose = async (
    target: Readonly<{
      declaredId: string
      address: { readonly id: string } | { readonly key: string }
    }>,
    options?: ActorOperationOptions,
  ): Promise<ActorOperationReceipt> => {
    if (options?.signal?.aborted) throw abortSignalReason(options.signal)
    const correlationId = randomUUID()
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "actorCloseRequested",
        value: create(runProto.ActorCloseRequestedSchema, {
          correlationId,
          declaredId: target.declaredId,
          address: actorAddress(target.address),
          ...(options?.idempotencyKey === undefined
            ? {}
            : { idempotencyKey: options.idempotencyKey }),
        }),
      })
      requireRuntimeOperationDecision(decision, correlationId, "Actor close")
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Actor close", decision.dataJson)
      }
      return parseRuntimeProtocolValue("Actor close result", () => {
        const value = parseObjectJSON(decision.dataJson, "Actor close result")
        requireExactKeys(value, ["accepted_at", "actor_id"], "Actor close result")
        const actorId = stringField(value, "actor_id", "Actor close result")
        const acceptedAt = new Date(
          stringField(value, "accepted_at", "Actor close result"),
        )
        if (!/^act_[a-z2-7]{26}$/.test(actorId) ||
          Number.isNaN(acceptedAt.getTime())) {
          throw new Error("Actor close result is invalid")
        }
        return Object.freeze({ actorId, acceptedAt })
      })
    })
    return abortableRuntimeOperation(operation, options?.signal)
  }
  const performActorOutputPage = async (
    target: Readonly<{
      declaredId: string
      address: { readonly id: string } | { readonly key: string }
    }>,
    options?: Readonly<{ after?: number; limit?: number; signal?: AbortSignal }>,
  ) => {
    if (options?.signal?.aborted) throw abortSignalReason(options.signal)
    const correlationId = randomUUID()
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "actorOutputPageRequested",
        value: create(runProto.ActorOutputPageRequestedSchema, {
          correlationId,
          declaredId: target.declaredId,
          address: actorAddress(target.address),
          ...(options?.after === undefined
            ? {}
            : { after: BigInt(options.after) }),
          limit: options?.limit ?? 50,
        }),
      })
      requireRuntimeOperationDecision(
        decision,
        correlationId,
        "Actor output page",
      )
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Actor output page", decision.dataJson)
      }
      return parseRuntimeProtocolValue("Actor output page result", () => {
        const value = parseObjectJSON(decision.dataJson, "Actor output page result")
        requireExactKeys(
          value,
          ["has_more", "next_after", "records"],
          "Actor output page result",
        )
        const records = value["records"]
        if (!Array.isArray(records)) {
          throw new Error("Actor output page result.records must be an array")
        }
        const hasMore = value["has_more"]
        if (typeof hasMore !== "boolean") {
          throw new Error("Actor output page result.has_more must be a boolean")
        }
        const nextAfter = safeJSONSequence(
          value["next_after"],
          "Actor output page result.next_after",
        )
        return Object.freeze({
          records: Object.freeze(records.map((record) =>
            parseActorOutputRecord(JSON.stringify(record))
          )),
          nextAfter,
          hasMore,
        })
      })
    })
    return abortableRuntimeOperation(operation, options?.signal)
  }
  const workspaceAddress = (
    address: Readonly<{ id: string } | { key: string }>,
  ) => "id" in address
    ? create(runProto.WorkspaceAddressSchema, {
        address: { case: "workspaceId", value: address.id },
      })
    : create(runProto.WorkspaceAddressSchema, {
        address: { case: "workspaceKey", value: address.key },
      })
  const performWorkspaceCreate = async (
    declaredId: string,
    options: import("@helmr/sdk").RuntimeWorkspaceCreateOptions = {},
  ): Promise<Readonly<{ workspaceId: string }>> => {
    if (options.signal?.aborted) throw abortSignalReason(options.signal)
    const correlationId = randomUUID()
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "workspaceCreateRequested",
        value: create(runProto.WorkspaceCreateRequestedSchema, {
          correlationId,
          declaredId,
          ...(options.key === undefined ? {} : { key: options.key }),
          secrets: options.secrets?.map((secret) =>
            create(runProto.WorkspaceSecretPlacementSchema, {
              name: secret.name,
              placement: "env" in secret
                ? { case: "env", value: secret.env }
                : { case: "file", value: secret.file },
            })
          ) ?? [],
          ...(options.idempotencyKey === undefined
            ? {}
            : { idempotencyKey: options.idempotencyKey }),
        }),
      })
      requireRuntimeOperationDecision(decision, correlationId, "Workspace create")
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Workspace create", decision.dataJson)
      }
      return parseRuntimeProtocolValue("Workspace create result", () => {
        const value = parseObjectJSON(decision.dataJson, "Workspace create result")
        requireExactKeys(value, ["workspace_id"], "Workspace create result")
        const workspaceId = stringField(
          value,
          "workspace_id",
          "Workspace create result",
        )
        if (!/^wsp_[a-z2-7]{26}$/.test(workspaceId)) {
          throw new Error("Workspace create result.workspace_id is invalid")
        }
        return Object.freeze({ workspaceId })
      })
    })
    return abortableRuntimeOperation(operation, options.signal)
  }
  const performWorkspaceRetrieve = async (
    address: Readonly<{ id: string } | { key: string }>,
    signal?: AbortSignal,
  ): Promise<WorkspaceSnapshot> => {
    if (signal?.aborted) throw abortSignalReason(signal)
    const correlationId = randomUUID()
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "workspaceRetrieveRequested",
        value: create(runProto.WorkspaceRetrieveRequestedSchema, {
          correlationId,
          workspace: workspaceAddress(address),
        }),
      })
      requireRuntimeOperationDecision(decision, correlationId, "Workspace retrieve")
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Workspace retrieve", decision.dataJson)
      }
      return parseRuntimeProtocolValue(
        "Workspace retrieve result",
        () => parseWorkspaceSnapshot(JSON.parse(decision.dataJson)),
      )
    })
    return abortableRuntimeOperation(operation, signal)
  }
  const performWorkspaceFileRead = async (
    address: Readonly<{ id: string } | { key: string }>,
    filePath: string,
    signal?: AbortSignal,
  ): Promise<Uint8Array> => {
    if (signal?.aborted) throw abortSignalReason(signal)
    const correlationId = randomUUID()
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "workspaceFileReadRequested",
        value: create(runProto.WorkspaceFileReadRequestedSchema, {
          correlationId,
          workspace: workspaceAddress(address),
          path: filePath,
        }),
      })
      requireRuntimeOperationDecision(decision, correlationId, "Workspace file read")
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Workspace file read", decision.dataJson)
      }
      return parseRuntimeProtocolValue(
        "Workspace file read result",
        () => parseWorkspaceFileContent(JSON.parse(decision.dataJson)),
      )
    })
    return abortableRuntimeOperation(operation, signal)
  }
  const performWorkspaceFileStat = async (
    address: Readonly<{ id: string } | { key: string }>,
    filePath: string,
    signal?: AbortSignal,
  ): Promise<WorkspaceFileEntry> => {
    if (signal?.aborted) throw abortSignalReason(signal)
    const correlationId = randomUUID()
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "workspaceFileStatRequested",
        value: create(runProto.WorkspaceFileStatRequestedSchema, {
          correlationId,
          workspace: workspaceAddress(address),
          path: filePath,
        }),
      })
      requireRuntimeOperationDecision(decision, correlationId, "Workspace file stat")
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Workspace file stat", decision.dataJson)
      }
      return parseRuntimeProtocolValue(
        "Workspace file stat result",
        () => parseWorkspaceFileEntry(JSON.parse(decision.dataJson)),
      )
    })
    return abortableRuntimeOperation(operation, signal)
  }
  const performWorkspaceFileList = async (
    address: Readonly<{ id: string } | { key: string }>,
    filePath: string,
    query: WorkspaceFileListQuery = {},
    signal?: AbortSignal,
  ) => {
    if (signal?.aborted) throw abortSignalReason(signal)
    const limit = query.limit ?? 50
    if (!Number.isInteger(limit) || limit < 1 || limit > 100) {
      throw new Error("Workspace file list limit must be an integer between 1 and 100")
    }
    const correlationId = randomUUID()
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "workspaceFileListRequested",
        value: create(runProto.WorkspaceFileListRequestedSchema, {
          correlationId,
          workspace: workspaceAddress(address),
          path: filePath,
          ...(query.cursor === undefined ? {} : { cursor: query.cursor }),
          limit,
        }),
      })
      requireRuntimeOperationDecision(decision, correlationId, "Workspace file list")
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Workspace file list", decision.dataJson)
      }
      return parseRuntimeProtocolValue(
        "Workspace file list result",
        () => parseWorkspaceFilePage(JSON.parse(decision.dataJson)),
      )
    })
    return abortableRuntimeOperation(operation, signal)
  }
  const performWorkspaceExec = async (
    address: Readonly<{ id: string } | { key: string }>,
    request: WorkspaceExecRequest,
    signal?: AbortSignal,
  ): Promise<WorkspaceExecResult> => {
    if (signal?.aborted) throw abortSignalReason(signal)
    const timeoutMs = request.timeout === undefined
      ? undefined
      : durationMilliseconds(request.timeout, "Workspace exec timeout")
    if (timeoutMs !== undefined && timeoutMs > 15 * 60 * 1_000) {
      throw new Error("Workspace exec timeout must not exceed 15m")
    }
    const correlationId = randomUUID()
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "workspaceExecRequested",
        value: create(runProto.WorkspaceExecRequestedSchema, {
          correlationId,
          workspace: workspaceAddress(address),
          command: [...request.command],
          ...(request.cwd === undefined ? {} : { cwd: request.cwd }),
          env: request.env === undefined ? {} : { ...request.env },
          stdin: request.stdin === undefined
            ? new Uint8Array()
            : new Uint8Array(request.stdin),
          ...(timeoutMs === undefined ? {} : { timeoutMs: BigInt(timeoutMs) }),
          idempotencyKey: request.idempotencyKey,
        }),
      })
      requireRuntimeOperationDecision(decision, correlationId, "Workspace exec")
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Workspace exec", decision.dataJson)
      }
      return parseRuntimeProtocolValue(
        "Workspace exec result",
        () => parseWorkspaceExecResult(JSON.parse(decision.dataJson)),
      )
    })
    return abortableRuntimeOperation(operation, signal)
  }
  const performWorkspaceDelete = async (
    address: Readonly<{ id: string } | { key: string }>,
    request: WorkspaceDeleteRequest = {},
    signal?: AbortSignal,
  ): Promise<WorkspaceDeleteReceipt> => {
    if (signal?.aborted) throw abortSignalReason(signal)
    const correlationId = randomUUID()
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "workspaceDeleteRequested",
        value: create(runProto.WorkspaceDeleteRequestedSchema, {
          correlationId,
          workspace: workspaceAddress(address),
          ...(request.idempotencyKey === undefined
            ? {}
            : { idempotencyKey: request.idempotencyKey }),
        }),
      })
      requireRuntimeOperationDecision(decision, correlationId, "Workspace delete")
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Workspace delete", decision.dataJson)
      }
      return parseRuntimeProtocolValue(
        "Workspace delete result",
        () => parseWorkspaceDeleteReceipt(JSON.parse(decision.dataJson)),
      )
    })
    return abortableRuntimeOperation(operation, signal)
  }
  const performTokenCreate = async (
    options: TokenCreateOptions,
  ): Promise<Omit<TokenCreateResult, "wait">> => {
    const correlationId = randomUUID()
    const timeoutMs = options.timeout === undefined
      ? undefined
      : durationMilliseconds(options.timeout, "Token timeout")
    const metadataJson = options.metadata === undefined
      ? undefined
      : new TextDecoder().decode(canonicalizeJsonValue(options.metadata))
    const idempotencyKey = normalizeTokenIdempotencyKey(options.idempotencyKey)
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "tokenCreateRequested",
        value: create(runProto.TokenCreateRequestedSchema, {
          correlationId,
          ...(timeoutMs === undefined ? {} : { timeoutMs: BigInt(timeoutMs) }),
          ...(idempotencyKey === undefined ? {} : { idempotencyKey }),
          tags: options.tags === undefined ? [] : [...options.tags],
          ...(metadataJson === undefined ? {} : { metadataJson }),
        }),
      })
      requireRuntimeOperationDecision(decision, correlationId, "Token create")
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Token create", decision.dataJson)
      }
      return parseRuntimeProtocolValue(
        "Token create result",
        () => parseTokenCreateResult(decision.dataJson),
      )
    })
    return await operation
  }
  const performTokenWait = async (
    tokenId: string,
    options: TokenWaitOptions,
  ): Promise<JsonValue> => {
    const releaseWait = waitGate.acquire()
    const correlationId = randomUUID()
    const timeoutMs = options.timeout === undefined
      ? undefined
      : durationMilliseconds(options.timeout, "Token Wait timeout")
    const idleTimeoutMs = options.idleTimeout === undefined
      ? undefined
      : tokenWaitIdleTimeoutMilliseconds(options.idleTimeout)
    try {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "runWaitRequested",
        value: create(runProto.RunWaitRequestedSchema, {
          correlationId,
          kind: "token",
          paramsJson: JSON.stringify({ token_id: tokenId }),
          ...(options.metadata === undefined
            ? {}
            : { metadataJson: new TextDecoder().decode(canonicalizeJsonValue(options.metadata)) }),
          ...(timeoutMs === undefined ? {} : { timeoutMs: BigInt(timeoutMs) }),
          ...(idleTimeoutMs === undefined ? {} : { idleTimeoutMs: BigInt(idleTimeoutMs) }),
          tags: options.tags === undefined ? [] : [...options.tags],
          ...(actorCursor === undefined
            ? {}
            : { actorSpeculativeInputSequence: actorCursor.value }),
        }),
      })
      if (
        (decision.correlationId || decision.runWaitId) !== correlationId ||
        (decision.kind !== "completed" &&
          decision.kind !== "failed" &&
          decision.kind !== "cancelled")
      ) {
        throw new RuntimeProtocolError("Token resume decision did not match the pending Wait")
      }
      if (decision.requireConsumedAck) {
        await writeRuntimeProtocolEvent(io, {
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
      if (decision.kind === "cancelled" && actorCursor !== undefined) {
        const failure = parseRuntimeProtocolValue(
          "Actor Token cancellation decision",
          () => resumeFailure(decision.dataJson),
        )
        throw runOperations.cancel(failure.reasonCode)
      }
      if (decision.kind !== "completed") {
        throw tokenWaitFailure(decision.kind, decision.dataJson)
      }
      return parseRuntimeProtocolValue(
        "Token completion result",
        () => JSON.parse(decision.dataJson) as JsonValue,
      )
    } finally {
      releaseWait()
    }
  }
  const performMetadataMutation = async (
    request:
      | Readonly<{ operation: "set"; key: string; value: JsonValue }>
      | Readonly<{ operation: "patch"; values: Metadata }>
      | Readonly<{ operation: "increment"; key: string; amount: number }>,
  ): Promise<void> => {
    const correlationId = randomUUID()
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "metadataUpdated",
        value: create(runProto.MetadataUpdatedSchema, {
          correlationId,
          operation: request.operation,
          ...(request.operation === "set"
            ? {
                key: normalizeMetadataKey(request.key),
                valueJson: new TextDecoder().decode(
                  canonicalizeJsonValue(request.value),
                ),
              }
            : request.operation === "patch"
            ? {
                patchJson: new TextDecoder().decode(
                  canonicalizeMetadataPatch(request.values),
                ),
              }
            : {
                key: normalizeMetadataKey(request.key),
                amount: finiteMetadataIncrement(request.amount),
              }),
        }),
      })
      requireRuntimeOperationDecision(decision, correlationId, "Metadata mutation")
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Metadata mutation", decision.dataJson)
      }
    })
    await operation
  }
  const performStructuredLog = async (
    level: RunLogLevel,
    message: string,
    attributes: LogAttributes,
  ): Promise<void> => {
    if (
      level !== "debug" &&
      level !== "info" &&
      level !== "warn" &&
      level !== "error"
    ) {
      throw new Error("logger level must be debug, info, warn, or error")
    }
    if (typeof message !== "string") {
      throw new Error("logger message must be a string")
    }
    if (new TextEncoder().encode(message).byteLength > MAX_RUN_LOG_MESSAGE_BYTES) {
      throw new Error(
        `logger message must be at most ${MAX_RUN_LOG_MESSAGE_BYTES} UTF-8 bytes`,
      )
    }
    const attributesJson = canonicalizeLogAttributes(attributes)
    const correlationId = randomUUID()
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "structuredLogRequested",
        value: create(runProto.StructuredLogRequestedSchema, {
          correlationId,
          level,
          message,
          attributesJson: new TextDecoder().decode(attributesJson),
        }),
      })
      requireRuntimeOperationDecision(decision, correlationId, "Structured log")
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Structured log", decision.dataJson)
      }
    })
    await operation
  }
  return {
    taskStart: performTaskStart,
    taskCall: performTaskCall,
    waitFor(duration) {
      return wait({ duration }, durationMilliseconds(duration))
    },
    waitUntil(date) {
      if (!(date instanceof Date) || Number.isNaN(date.getTime())) {
        return Promise.reject(new Error("timers.waitUntil() requires a valid Date"))
      }
      const remainingMs = date.getTime() - Date.now()
      if (remainingMs <= 0) return Promise.resolve()
      return wait(
        { date: date.toISOString() },
        boundedTimerMilliseconds(Math.ceil(remainingMs)),
      )
    },
    actorInputSend(target, input, options) {
      return performActorInputSend(target, input, options)
    },
    actorStart(declaredId, options) {
      return performActorStart(declaredId, options)
    },
    actorStatus(target) {
      return runOperations.trackDrainable(() => performActorStatus(target))
    },
    actorClose(target, options) {
      return performActorClose(target, options)
    },
    actorOutputPage(target, options) {
      return performActorOutputPage(target, options)
    },
    workspaceCreate(declaredId, options) {
      return performWorkspaceCreate(declaredId, options)
    },
    workspaceRetrieve(address, signal) {
      return performWorkspaceRetrieve(address, signal)
    },
    workspaceFileRead(address, path, signal) {
      return performWorkspaceFileRead(address, path, signal)
    },
    workspaceFileStat(address, path, signal) {
      return performWorkspaceFileStat(address, path, signal)
    },
    workspaceFileList(address, path, query, signal) {
      return performWorkspaceFileList(address, path, query, signal)
    },
    workspaceExec(address, request, signal) {
      return performWorkspaceExec(address, request, signal)
    },
    workspaceDelete(address, request, signal) {
      return performWorkspaceDelete(address, request, signal)
    },
    tokenCreate(options) {
      return performTokenCreate(options)
    },
    tokenWait(tokenId, options) {
      return runOperations.track(() => performTokenWait(tokenId, options))
    },
    metadataSet(key, value) {
      return performMetadataMutation({ operation: "set", key, value })
    },
    metadataPatch(values) {
      return performMetadataMutation({ operation: "patch", values })
    },
    metadataIncrement(key, amount) {
      return performMetadataMutation({ operation: "increment", key, amount })
    },
    structuredLog(level, message, attributes) {
      return performStructuredLog(level, message, attributes)
    },
  }
}

function normalizeMetadataKey(value: string): string {
  if (typeof value !== "string" || value === "") {
    throw new Error("metadata key must be a nonempty string")
  }
  if (new TextEncoder().encode(value).byteLength > 512) {
    throw new Error("metadata key must be at most 512 UTF-8 bytes")
  }
  return value
}

function canonicalizeMetadataPatch(values: Metadata): Uint8Array {
  if (values === null || typeof values !== "object" || Array.isArray(values)) {
    throw new Error("metadata.patch() requires an object")
  }
  for (const key of Object.keys(values)) normalizeMetadataKey(key)
  return canonicalizeJsonValue(values)
}

function finiteMetadataIncrement(value: number): number {
  if (typeof value !== "number" || !Number.isFinite(value)) {
    throw new Error("metadata.increment() amount must be finite")
  }
  return value
}

function canonicalizeLogAttributes(attributes: LogAttributes): Uint8Array {
  if (
    attributes === null ||
    typeof attributes !== "object" ||
    Array.isArray(attributes)
  ) {
    throw new Error("logger attributes must be an object")
  }
  const normalized = canonicalizeJsonValue(attributes)
  if (normalized.byteLength > MAX_RUN_LOG_ATTRIBUTES_BYTES) {
    throw new Error(
      `logger attributes must be at most ${MAX_RUN_LOG_ATTRIBUTES_BYTES} canonical JSON bytes`,
    )
  }
  return normalized
}

function normalizeTokenIdempotencyKey(
  value: string | undefined,
): string | undefined {
  if (value === undefined) return undefined
  const normalized = trimGoSpace(value)
  if (new TextEncoder().encode(normalized).byteLength > 512) {
    throw new Error("Token idempotency key must be at most 512 UTF-8 bytes")
  }
  return normalized === "" ? undefined : normalized
}

function taskRetryRequest(retry: RetryPolicy): JsonValue {
  if (retry.enabled === false) return { enabled: false }
  return {
    ...(retry.enabled === undefined ? {} : { enabled: retry.enabled }),
    max_attempts: retry.maxAttempts,
    ...(retry.backoff === undefined
      ? {}
      : {
          backoff: {
            ...(retry.backoff.minDelay === undefined
              ? {}
              : { min_delay: retry.backoff.minDelay }),
            ...(retry.backoff.maxDelay === undefined
              ? {}
              : { max_delay: retry.backoff.maxDelay }),
            ...(retry.backoff.factor === undefined
              ? {}
              : { factor: retry.backoff.factor }),
            ...(retry.backoff.jitter === undefined
              ? {}
              : { jitter: retry.backoff.jitter }),
          },
        }),
  }
}

function parseTokenCreateResult(dataJson: string): Omit<TokenCreateResult, "wait"> {
  const value = parseObjectJSON(dataJson, "Token create result")
  const metadata = objectField(value, "metadata", "Token create result") as Metadata
  const tags = value["tags"]
  if (!Array.isArray(tags) || tags.some((tag) => typeof tag !== "string")) {
    throw new Error("Token create result.tags must be an array of strings")
  }
  if (value["status"] !== "pending") {
    throw new Error("Token create result.status must be pending")
  }
  return Object.freeze({
    id: stringField(value, "id", "Token create result"),
    callbackUrl: stringField(value, "callback_url", "Token create result"),
    publicAccessToken: stringField(value, "public_access_token", "Token create result"),
    timeoutAt: stringField(value, "timeout_at", "Token create result"),
    status: "pending" as const,
    metadata,
    tags: Object.freeze([...tags]) as readonly string[],
    createdAt: stringField(value, "created_at", "Token create result"),
    updatedAt: stringField(value, "updated_at", "Token create result"),
  })
}

function runtimeOperationFailure(operation: string, dataJson: string): Error {
  const value = parseObjectJSON(dataJson, `${operation} failure`)
  const code = stringField(value, "code", `${operation} failure`)
  const message = stringField(value, "message", `${operation} failure`)
  const retryable = value["retryable"]
  if (typeof retryable !== "boolean") {
    throw new Error(`${operation} failure.retryable must be a boolean`)
  }
  const error = new Error(message) as Error & { code: string; retryable: boolean }
  error.name = "HelmrError"
  error.code = code
  error.retryable = retryable
  return error
}

function parseTaskResult(dataJson: string): TaskResult<JsonValue> {
  const value = parseObjectJSON(dataJson, "Task child call result")
  const ok = value["ok"]
  if (ok === true) {
    requireExactKeys(
      value,
      ["ok", "output", "run"],
      "Task child call success",
    )
    return Object.freeze({
      ok: true,
      output: jsonValueField(value, "output", "Task child call success"),
      run: parseTaskResultRun(value),
    })
  }
  if (ok !== false) {
    throw new Error("Task child call result.ok must be a boolean")
  }
  requireExactKeys(
    value,
    ["error", "ok", "run"],
    "Task child call failure",
  )
  const rawError = objectField(value, "error", "Task child call failure")
  const errorKeys = Object.keys(rawError)
  requireExactKeys(
    rawError,
    errorKeys.includes("details")
      ? ["code", "details", "message", "retryable"]
      : ["code", "message", "retryable"],
    "Task child call failure.error",
  )
  const retryable = rawError["retryable"]
  if (typeof retryable !== "boolean") {
    throw new Error(
      "Task child call failure.error.retryable must be a boolean",
    )
  }
  const error = new Error(
    stringField(rawError, "message", "Task child call failure.error"),
  ) as Error & {
    code: string
    retryable: boolean
    details?: JsonValue
  }
  error.name = "HelmrError"
  error.code = stringField(
    rawError,
    "code",
    "Task child call failure.error",
  )
  error.retryable = retryable
  if (errorKeys.includes("details")) {
    error.details = jsonValueField(
      rawError,
      "details",
      "Task child call failure.error",
    )
  }
  return Object.freeze({
    ok: false,
    error: Object.freeze(error),
    run: parseTaskResultRun(value),
  })
}

function parseTaskResultRun(
  value: Record<string, unknown>,
): Readonly<{ id: string }> {
  const run = objectField(value, "run", "Task child call result")
  requireExactKeys(run, ["id"], "Task child call result.run")
  const id = stringField(run, "id", "Task child call result.run")
  if (!/^run_[a-z2-7]{26}$/.test(id)) {
    throw new Error(
      "Task child call result.run.id must be a canonical Run public ID",
    )
  }
  return Object.freeze({ id })
}

function tokenWaitFailure(kind: "failed" | "cancelled", dataJson: string): Error {
  const failure = parseRuntimeProtocolValue(
    "Token Wait failure",
    () => resumeFailure(dataJson),
  )
  const code = failure.reasonCode
  const error = new Error(
    code === "wait_timeout"
      ? "Token wait timed out"
      : code === "token_expired"
      ? "Token expired"
      : code === "token_cancelled"
      ? "Token was cancelled"
      : `Token Wait ${kind}: ${code}`,
  ) as Error & { code: string; retryable: false }
  error.name = code === "wait_timeout" ? "WaitTimeoutError" : "HelmrError"
  error.code = code
  error.retryable = false
  return error
}

function normalizeActorInputIdempotencyKey(
  value: string | undefined,
): string | undefined {
  if (value === undefined) return undefined
  const normalized = trimGoSpace(value)
  if (new TextEncoder().encode(normalized).byteLength > 512) {
    throw actorInputSendError(
      "invalid_idempotency_key",
      "Actor input idempotency key must be at most 512 UTF-8 bytes",
      false,
    )
  }
  return normalized === "" ? undefined : normalized
}

function requireRuntimeOperationDecision(
  decision: runProto.ResumeDecision,
  correlationId: string,
  operation: string,
): void {
  if (
    decision.correlationId !== correlationId ||
    (decision.kind !== "completed" && decision.kind !== "failed") ||
    decision.runWaitId !== "" ||
    decision.requireConsumedAck ||
    decision.checkpointId !== "" ||
    decision.resumeAttachId !== "" ||
    decision.resumeRequestVersion !== 0n ||
    decision.runLeaseId !== "" ||
    decision.noResult
  ) {
    throw new RuntimeProtocolError(
      `${operation} decision did not match the pending operation`,
    )
  }
}

function parseActorInputSendResult(
  dataJson: string,
): { readonly sequence: number } {
  const value = parseObjectJSON(dataJson, "Actor input send result")
  requireExactKeys(value, ["sequence"], "Actor input send result")
  const sequence = safeJSONSequence(
    value["sequence"],
    "Actor input send result.sequence",
  )
  if (sequence === 0) {
    throw new Error("Actor input send result.sequence must be positive")
  }
  return Object.freeze({ sequence })
}

function parseActorInputSendFailure(dataJson: string): Error {
  const value = parseObjectJSON(dataJson, "Actor input send failure")
  requireExactKeys(
    value,
    ["code", "message", "retryable"],
    "Actor input send failure",
  )
  if (
    typeof value["code"] !== "string" ||
    value["code"].trim() === "" ||
    typeof value["message"] !== "string" ||
    value["message"].trim() === "" ||
    typeof value["retryable"] !== "boolean"
  ) {
    throw new Error(
      "Actor input send failure must contain code, message, and retryable",
    )
  }
  return actorInputSendError(
    value["code"],
    value["message"],
    value["retryable"],
  )
}

function actorInputSendError(
  code: string,
  message: string,
  retryable: boolean,
): Error {
  const error = new Error(message) as Error & {
    code: string
    retryable: boolean
  }
  error.name = "HelmrError"
  error.code = code
  error.retryable = retryable
  return error
}

async function abortableRuntimeOperation<T>(
  operation: Promise<T>,
  signal: AbortSignal | undefined,
): Promise<T> {
  if (signal === undefined) return operation
  if (signal.aborted) throw abortSignalReason(signal)
  let rejectAbort: ((reason?: unknown) => void) | undefined
  const aborted = new Promise<never>((_resolve, reject) => {
    rejectAbort = reject
  })
  const onAbort = () => rejectAbort?.(abortSignalReason(signal))
  signal.addEventListener("abort", onAbort, { once: true })
  try {
    return await Promise.race([operation, aborted])
  } finally {
    signal.removeEventListener("abort", onAbort)
  }
}

function abortSignalReason(signal: AbortSignal): unknown {
  return signal.reason === undefined
    ? new DOMException("The operation was aborted", "AbortError")
    : signal.reason
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

function durationMilliseconds(duration: string, label = "timer duration"): number {
  const match = /^([1-9][0-9]*)(ms|s|m|h|d)$/.exec(duration)
  if (match === null) {
    throw new Error(
      `${label} must be a positive integer followed by ms, s, m, h, or d`,
    )
  }
  const amount = BigInt(match[1]!)
  const unit = match[2]
  const multiplierMs = unit === "ms"
    ? 1n
    : unit === "s"
    ? 1000n
    : unit === "m"
    ? 60_000n
    : unit === "h"
    ? 3_600_000n
    : 86_400_000n
  const milliseconds = amount * multiplierMs
  const maxMilliseconds = 365n * 24n * 60n * 60n * 1000n
  if (milliseconds > maxMilliseconds) {
    throw new Error(`${label} must be between 1ms and 365d`)
  }
  return boundedTimerMilliseconds(Number(milliseconds))
}

function boundedTimerMilliseconds(milliseconds: number): number {
  const maxMilliseconds = 365 * 24 * 60 * 60 * 1000
  if (!Number.isSafeInteger(milliseconds) || milliseconds < 1 || milliseconds > maxMilliseconds) {
    throw new Error("timer duration must be between 1ms and 365d")
  }
  return milliseconds
}

function tokenWaitIdleTimeoutMilliseconds(duration: Duration): number {
  const milliseconds = durationMilliseconds(duration, "Token Wait idle timeout")
  if (milliseconds > 60 * 60 * 1000) {
    throw new Error("Token Wait idle timeout must be between 1ms and 1h")
  }
  return milliseconds
}

async function runActor(
  start: runProto.ProgramStart,
  definition: InternalActorDefinition,
  io: ProgramIO,
  decisions: ResumeDecisionRouter,
): Promise<void> {
  if (start.entrypoint.case !== "actor") {
    throw new Error("Actor Program-start entrypoint is required")
  }
  const cursor = { value: start.entrypoint.value.startInputSequence }
  const waitGate = new ConsumingWaitGate()
  const actorOperations = new RunOperationState()
  const uninstallRuntime = installRuntimeOperations(
    programRuntimeOperations(start, io, decisions, waitGate, actorOperations, cursor),
  )
  try {
    await definition.handler(
      actorSelf(start, io, decisions, cursor, waitGate, actorOperations),
      actorContext(start, actorOperations.controller.signal),
    )
    try {
      await actorOperations.drainForCompletion()
      actorOperations.assertCanComplete()
    } catch (error) {
      decisions.abandonPending()
      throw error
    }
  } catch (error) {
    if (error instanceof RuntimeProtocolError || error instanceof ActorCancellationError) {
      throw error
    }
    await actorOperations.drainForCompletion()
    actorOperations.assertCanComplete()
    await writeActorFailure(
      io,
      cursor.value,
      errorMessage(error),
    )
    return
  } finally {
    uninstallRuntime()
  }
  await writeRunEvent(io, {
    case: "actorOutcome",
    value: create(runProto.ActorOutcomeSchema, {
      terminalInputSequence: cursor.value,
      outcome: {
        case: "succeeded",
        value: create(runProto.ActorSucceededSchema),
      },
    }),
  })
}

function actorSelf(
  start: runProto.ProgramStart,
  io: ProgramIO,
  decisions: ResumeDecisionRouter,
  cursor: { value: bigint },
  waitGate: ConsumingWaitGate,
  actorOperations: RunOperationState,
): ActorSelf {
  if (start.entrypoint.case !== "actor") {
    throw new Error("Actor Program-start entrypoint is required")
  }
  const actorStart = start.entrypoint.value
  let committedBoundary = cursor.value

  const commitPriorTurn = async (): Promise<void> => {
    if (cursor.value === committedBoundary) return
    const correlationId = randomUUID()
    const decision = await requestRuntimeDecision(io, decisions, correlationId, {
      case: "actorTurnCommitRequested",
      value: create(runProto.ActorTurnCommitRequestedSchema, {
        correlationId,
        targetInputSequence: cursor.value,
      }),
    })
    requireActorDecision(decision, correlationId, "committed", "Actor turn commit")
    committedBoundary = cursor.value
  }

  const performReceive = async (
    options: ReceiveOptions | undefined,
    releaseWait: () => void,
  ): Promise<ActorInputResult> => {
    try {
      await commitPriorTurn()
      const correlationId = randomUUID()
      const timeoutMs = options?.timeout === undefined
        ? undefined
        : durationMilliseconds(options.timeout)
      const idleTimeoutMs = options?.idleTimeout === undefined
        ? undefined
        : durationMilliseconds(options.idleTimeout)
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "runWaitRequested",
        value: create(runProto.RunWaitRequestedSchema, {
          correlationId,
          kind: "actor_input",
          paramsJson: JSON.stringify({
            actor_id: actorStart.actorId,
            after_input_sequence: safeActorSequence(cursor.value),
          }),
          ...(options?.metadata === undefined
            ? {}
            : { metadataJson: new TextDecoder().decode(canonicalizeJsonValue(options.metadata)) }),
          ...(timeoutMs === undefined ? {} : { timeoutMs: BigInt(timeoutMs) }),
          ...(idleTimeoutMs === undefined ? {} : { idleTimeoutMs: BigInt(idleTimeoutMs) }),
          tags: options?.tags === undefined ? [] : [...options.tags],
          actorSpeculativeInputSequence: cursor.value,
        }),
      })
      if ((decision.correlationId || decision.runWaitId) !== correlationId) {
        throw new RuntimeProtocolError("Actor input resume decision did not match the pending receive")
      }
      if (decision.requireConsumedAck) {
        await writeRuntimeProtocolEvent(io, {
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
      if (decision.kind === "completed") {
        const delivered = parseRuntimeProtocolValue(
          "Actor input delivery",
          () => parseActorInputDelivery(decision.dataJson),
        )
        if (BigInt(delivered.record.sequence) !== cursor.value + 1n) {
          throw new RuntimeProtocolError("Actor input delivery was not the next contiguous record")
        }
        cursor.value = BigInt(delivered.record.sequence)
        return delivered
      }
      if (decision.kind === "failed") {
        const failure = parseRuntimeProtocolValue(
          "Actor input Wait failure decision",
          () => resumeFailure(decision.dataJson),
        )
        if (failure.reasonCode !== "wait_timeout" && failure.reasonCode !== "actor_closed") {
          throw new RuntimeProtocolError(`Actor input Wait failed: ${failure.reasonCode}`)
        }
        return Object.freeze({
          ok: false,
          error: actorChannelError(failure.reasonCode),
        })
      }
      if (decision.kind === "cancelled") {
        const failure = parseRuntimeProtocolValue(
          "Actor input cancellation decision",
          () => resumeFailure(decision.dataJson),
        )
        throw actorOperations.cancel(failure.reasonCode)
      }
      throw new RuntimeProtocolError(`Actor input Wait returned unsupported decision ${decision.kind}`)
    } finally {
      releaseWait()
    }
  }

  const receive = (options?: ReceiveOptions): ActorReceive => {
    let releaseWait: () => void
    try {
      releaseWait = waitGate.acquire(concurrentActorReceiveError)
    } catch (error) {
      return actorReceive(Promise.reject(concurrentActorReceiveError()))
    }
    return actorReceive(actorOperations.track(() => performReceive(options, releaseWait)))
  }

  const performAppend = async (
    value: Serializable,
    options?: OutputAppendOptions,
  ): Promise<ActorOutputRecord> => {
    const normalized = canonicalizeJsonValue(value as JsonValue)
    const correlationId = randomUUID()
    const decision = await requestRuntimeDecision(io, decisions, correlationId, {
      case: "actorOutputAppendRequested",
      value: create(runProto.ActorOutputAppendRequestedSchema, {
        correlationId,
        dataJson: new TextDecoder().decode(normalized),
        contentType: options?.contentType ?? "application/json",
        ...(options?.idempotencyKey === undefined
          ? {}
          : { idempotencyKey: options.idempotencyKey }),
      }),
    })
    requireRuntimeOperationDecision(decision, correlationId, "Actor output append")
    if (decision.kind === "failed") {
      throw runtimeOperationFailure("Actor output append", decision.dataJson)
    }
    return parseRuntimeProtocolValue(
      "Actor output append result",
      () => parseActorOutputRecord(decision.dataJson),
    )
  }

  const append = (
    value: Serializable,
    options?: OutputAppendOptions,
  ): Promise<ActorOutputRecord> => actorOperations.track(
    () => performAppend(value, options),
  )

  const performPipe = async (
    source: AsyncIterable<Serializable> | Iterable<Serializable>,
    options?: OutputSequenceOptions,
  ): Promise<void> => {
    for await (const value of source) await performAppend(value, options)
  }

  const pipe = (
    source: AsyncIterable<Serializable> | Iterable<Serializable>,
    options?: OutputSequenceOptions,
  ): Promise<void> => actorOperations.track(() => performPipe(source, options))

  const writer = (options?: OutputSequenceOptions) => {
    let closed = false
    return Object.freeze({
      write(value: Serializable): Promise<ActorOutputRecord> {
        if (closed) return Promise.reject(new Error("Actor output writer is closed"))
        return append(value, options)
      },
      async close(): Promise<void> { closed = true },
    })
  }

  return Object.freeze({
    input: Object.freeze({ receive }),
    output: Object.freeze({
      append,
      pipe,
      writer,
    }),
  })
}

function actorReceive(result: Promise<ActorInputResult>): ActorReceive {
  return Object.freeze({
    then: result.then.bind(result),
    async unwrap(): Promise<JsonValue> {
      const resolved = await result
      if (resolved.ok) return resolved.value
      throw resolved.error
    },
  }) as ActorReceive
}

function requireActorDecision(
  decision: runProto.ResumeDecision,
  correlationId: string,
  kind: string,
  operation: string,
): void {
  if ((decision.correlationId || decision.runWaitId) !== correlationId || decision.kind !== kind) {
    throw new RuntimeProtocolError(`${operation} decision did not match the pending operation`)
  }
}

function safeActorSequence(value: bigint): number {
  if (value < 0n || value > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new Error("Actor input sequence exceeds the JavaScript safe-integer range")
  }
  return Number(value)
}

function parseActorInputDelivery(dataJson: string): Extract<ActorInputResult, { ok: true }> {
  const value = parseObjectJSON(dataJson, "Actor input delivery")
  requireExactKeys(value, ["record", "value"], "Actor input delivery")
  const record = objectField(value, "record", "Actor input delivery")
  requireExactKeys(record, ["created_at", "id", "sequence", "source"], "Actor input record")
  const source = objectField(record, "source", "Actor input record")
  const sourceType = stringField(source, "type", "Actor input source")
  let parsedSource: { readonly type: "external" } | {
    readonly type: "run"
    readonly runId: string
  }
  if (sourceType === "external") {
    requireExactKeys(source, ["type"], "Actor input source")
    parsedSource = Object.freeze({ type: "external" })
  } else if (sourceType === "run") {
    requireExactKeys(source, ["run_id", "type"], "Actor input source")
    parsedSource = Object.freeze({
      type: "run",
      runId: stringField(source, "run_id", "Actor input source"),
    })
  } else {
    throw new Error("Actor input source type is invalid")
  }
  const sequence = safeJSONSequence(record["sequence"], "Actor input record.sequence")
  return Object.freeze({
    ok: true,
    value: jsonValueField(value, "value", "Actor input delivery"),
    record: Object.freeze({
      id: stringField(record, "id", "Actor input record"),
      sequence,
      createdAt: stringField(record, "created_at", "Actor input record"),
      source: parsedSource,
    }),
  })
}

function parseActorOutputRecord(dataJson: string): ActorOutputRecord {
  const value = parseObjectJSON(dataJson, "Actor output append result")
  requireExactKeys(
    value,
    ["content_type", "created_at", "data", "id", "provenance", "sequence"],
    "Actor output append result",
  )
  const provenance = objectField(value, "provenance", "Actor output append result")
  requireExactKeys(
    provenance,
    ["attempt_number", "deployment_id", "run_id"],
    "Actor output provenance",
  )
  return Object.freeze({
    id: stringField(value, "id", "Actor output append result"),
    sequence: safeJSONSequence(value["sequence"], "Actor output sequence"),
    data: jsonValueField(value, "data", "Actor output append result"),
    contentType: stringField(value, "content_type", "Actor output append result"),
    createdAt: stringField(value, "created_at", "Actor output append result"),
    provenance: Object.freeze({
      runId: stringField(provenance, "run_id", "Actor output provenance"),
      attemptNumber: safeJSONSequence(
        provenance["attempt_number"],
        "Actor output attempt number",
      ),
      deploymentId: stringField(
        provenance,
        "deployment_id",
        "Actor output provenance",
      ),
    }),
  })
}

function parseActorStatus(dataJson: string): ActorStatus {
  const value = parseObjectJSON(dataJson, "Actor status result")
  const optional = ["current_run_id", "failure", "key"]
  const required = ["created_at", "id", "status", "updated_at"]
  const actual = Object.keys(value).sort()
  const allowed = new Set([...required, ...optional])
  if (
    required.some((key) => !Object.hasOwn(value, key)) ||
    actual.some((key) => !allowed.has(key))
  ) {
    throw new Error("Actor status result has unknown or missing fields")
  }
  const id = stringField(value, "id", "Actor status result")
  if (!/^act_[a-z2-7]{26}$/.test(id)) {
    throw new Error("Actor status result.id is invalid")
  }
  const status = stringField(value, "status", "Actor status result")
  if (
    status !== "open" && status !== "closed" &&
    status !== "cancelled" && status !== "failed"
  ) {
    throw new Error("Actor status result.status is invalid")
  }
  const createdAt = new Date(
    stringField(value, "created_at", "Actor status result"),
  )
  const updatedAt = new Date(
    stringField(value, "updated_at", "Actor status result"),
  )
  if (Number.isNaN(createdAt.getTime()) || Number.isNaN(updatedAt.getTime())) {
    throw new Error("Actor status result timestamps are invalid")
  }
  let failure: ActorStatus["failure"]
  if (Object.hasOwn(value, "failure")) {
    const raw = objectField(value, "failure", "Actor status result")
    requireExactKeys(raw, ["code", "run_id"], "Actor status failure")
    const code = stringField(raw, "code", "Actor status failure")
    if (
      code !== "no-progress" && code !== "run-failed" &&
      code !== "run-expired" && code !== "platform-failure"
    ) {
      throw new Error("Actor status failure.code is invalid")
    }
    failure = Object.freeze({
      code,
      runId: stringField(raw, "run_id", "Actor status failure"),
    })
  }
  return Object.freeze({
    id,
    status,
    createdAt,
    updatedAt,
    ...(Object.hasOwn(value, "key")
      ? { key: stringField(value, "key", "Actor status result") }
      : {}),
    ...(Object.hasOwn(value, "current_run_id")
      ? {
          currentRunId: stringField(
            value,
            "current_run_id",
            "Actor status result",
          ),
        }
      : {}),
    ...(failure === undefined ? {} : { failure }),
  })
}

function parseObjectJSON(value: string, label: string): Record<string, unknown> {
  let parsed: unknown
  try {
    parsed = JSON.parse(value)
  } catch {
    throw new Error(`${label} must be valid JSON`)
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error(`${label} must be an object`)
  }
  return parsed as Record<string, unknown>
}

function objectField(
  value: Record<string, unknown>,
  field: string,
  label: string,
): Record<string, unknown> {
  const nested = value[field]
  if (nested === null || typeof nested !== "object" || Array.isArray(nested)) {
    throw new Error(`${label}.${field} must be an object`)
  }
  return nested as Record<string, unknown>
}

function requireExactKeys(
  value: Record<string, unknown>,
  expected: readonly string[],
  label: string,
): void {
  const actual = Object.keys(value).sort()
  if (actual.length !== expected.length || actual.some((key, index) => key !== expected[index])) {
    throw new Error(`${label} has unknown or missing fields`)
  }
}

function stringField(
  value: Record<string, unknown>,
  field: string,
  label: string,
): string {
  const result = value[field]
  if (typeof result !== "string" || result.trim() === "") {
    throw new Error(`${label}.${field} must be a non-empty string`)
  }
  return result
}

function jsonValueField(
  value: Record<string, unknown>,
  field: string,
  label: string,
): JsonValue {
  const result = value[field]
  try {
    canonicalizeJsonValue(result as JsonValue)
  } catch (error) {
    throw new Error(`${label}.${field} must be a JSON value`, { cause: error })
  }
  return result as JsonValue
}

function safeJSONSequence(value: unknown, label: string): number {
  if (!Number.isSafeInteger(value) || (value as number) < 0) {
    throw new Error(`${label} must be a non-negative safe integer`)
  }
  return value as number
}

function actorChannelError(
  code: "wait_timeout" | "actor_closed",
): Extract<ActorInputResult, { ok: false }>["error"] {
  const error = new Error(code === "wait_timeout" ? "Actor input receive timed out" : "Actor is closed") as Error & {
    code: "wait_timeout" | "actor_closed"
    retryable: false
  }
  error.name = code === "wait_timeout" ? "WaitTimeoutError" : "ActorClosedError"
  error.code = code
  error.retryable = false
  return error as Extract<ActorInputResult, { ok: false }>["error"]
}

function concurrentActorReceiveError(): Error {
  const error = new Error("only one Actor input receive may be unresolved")
  error.name = "ConcurrentActorReceiveError"
  return error
}

function actorContext(
  start: runProto.ProgramStart,
  signal: AbortSignal,
): ActorExecutionContext {
  if (start.entrypoint.case !== "actor") {
    throw new Error("Actor Program-start entrypoint is required")
  }
  return Object.freeze({
    ...taskContext(start, signal),
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

function taskContext(
  start: runProto.ProgramStart,
  signal = new AbortController().signal,
): TaskExecutionContext {
  return Object.freeze({
    signal,
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
