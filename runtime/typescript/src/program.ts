import { create, fromBinary, toBinary } from "@bufbuild/protobuf"
import { programProto } from "@helmr/proto"
import {
  canonicalizeJsonValue,
  createWorkspaceRef,
  createRunHandle,
  inspectDefinition,
  encodeWorkspaceSecrets,
  installRuntimeOperations,
  parseWorkspaceDeleteReceipt,
  parseWorkspaceExecResult,
  parseWorkspace,
  parseSession,
  parseSessionAdmissionReceipt,
  parseSessionMessageReceipt,
  parseSessionCloseReceipt, parseSessionCancelReceipt,
  parseSessionResumeReceipt,
  parseSessionEventPage,
  parseTurnState,
  parseTurnInterruptReceipt,
  parseOutputReceipt,
  MessageRejected,
  workspaceRefID,
  resourceID,
  timestampString,
  trimGoSpace,
  type InternalActorDefinition,
  type InternalTaskDefinition,
  type RuntimeOperations,
} from "@helmr/sdk/internal"
import type {
  ActorContext,
  Turn,
  TurnSource,
  Message,
  RecordWriter,
  OutputReceipt,
  SessionOperationOptions,
  ActorSessionReceiveOptions,
  ActorStartOptions,
  Session,
  Duration,
  JsonValue,
  LogAttributes,
  Metadata,
  RunCause,
  RunLogLevel,
  RetryPolicy,
  TaskCallOptions,
  TaskContext,
  TaskResult,
  TokenCreateRequest,
  TokenCreateResult,
  TokenWaitOptions,
  WorkspaceDeleteReceipt,
  WorkspaceDeleteRequest,
  WorkspaceExecRequest,
  WorkspaceExecResult,
  Workspace,
  WorkspaceCreateRequest,
} from "@helmr/sdk"
import { createWriteStream, promises as fs } from "node:fs"
import { randomUUIDv7 as newUUIDv7 } from "node:crypto"
import { AsyncLocalStorage } from "node:async_hooks"
import { Readable } from "node:stream"
import path from "node:path"
import { fileURLToPath, pathToFileURL } from "node:url"

const MAX_PROGRAM_FRAME_BYTES = 256 * 1024 * 1024
const MAX_TASK_OUTPUT_BYTES = 16 * 1024 * 1024
const MAX_TASK_ERROR_BYTES = 16 * 1024
const MAX_RUN_LOG_MESSAGE_BYTES = 4 * 1024

type ExecutionContext = Omit<TaskContext, "task" | "actor">
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

interface ProgramLocator {
  readonly exportName: string
  readonly sourcePath: string
  readonly slot: "handler"
}

interface ProgramIndexDeclaration {
  readonly declaredId: string
  readonly kind: "task" | "actor" | "sandbox"
  readonly locator?: ProgramLocator
}

interface ProgramIndex {
  readonly declarations: readonly ProgramIndexDeclaration[]
}

class FrameReader {
  readonly #iterator: AsyncIterator<InputChunk>
  readonly #stream: Readable | undefined
  #closePromise: Promise<void> | undefined
  #chunk: Uint8Array<ArrayBufferLike> = new Uint8Array()
  #offset = 0

  constructor(input: AsyncIterable<InputChunk>) {
    this.#iterator = input[Symbol.asyncIterator]()
    this.#stream = input instanceof Readable ? input : undefined
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

  close(): Promise<void> {
    if (this.#closePromise !== undefined) return this.#closePromise
    this.#closePromise = this.#closeIterator()
    return this.#closePromise
  }

  async #closeIterator(): Promise<void> {
    // Node queues iterator.return() behind a pending next(). Stop the owned
    // stream first so the independent control reader can finish.
    this.#stream?.destroy()
    const close = this.#iterator.return
    if (close !== undefined) await close.call(this.#iterator)
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
    readonly resolve: (decision: programProto.ResumeDecision) => void
    readonly reject: (error: Error) => void
  }>()
  #reading = false
  #control: ((decision: programProto.ResumeDecision) => void) | undefined
  #controlFailure: ((error: RuntimeProtocolError) => void) | undefined

  constructor(reader: FrameReader) {
    this.#reader = reader
  }

  register(correlationId: string): Promise<programProto.ResumeDecision> {
    if (this.#pending.has(correlationId)) {
      return Promise.reject(new Error("duplicate runtime correlation id"))
    }
    const { promise, resolve, reject } =
      Promise.withResolvers<programProto.ResumeDecision>()
    this.#pending.set(correlationId, { resolve, reject })
    this.#pump()
    return promise
  }

  cancel(correlationId: string): void {
    this.#pending.delete(correlationId)
  }

  abandonPending(): void {
    this.#pending.clear()
  }

  listenForControl(
    control: (decision: programProto.ResumeDecision) => void,
    failed: (error: RuntimeProtocolError) => void,
  ): void {
    this.#control = control
    this.#controlFailure = failed
    this.#pump()
  }

  stopControl(): void {
    this.#control = undefined
    this.#controlFailure = undefined
  }

  #pump(): void {
    if (this.#reading) return
    this.#reading = true
    void (async () => {
      try {
        while (this.#pending.size > 0 || this.#control !== undefined) {
          const decision = fromBinary(
            programProto.ResumeDecisionSchema,
            await this.#reader.read(),
          )
          if (decision.kind === "session_stop") {
            if (this.#control === undefined) {
              throw new RuntimeProtocolError("Session stop has no Actor execution owner")
            }
            this.#control(decision)
            continue
          }
          const pending = this.#pending.get(decision.correlationId)
          if (pending === undefined) {
            throw new Error("resume decision did not match a pending runtime operation")
          }
          this.#pending.delete(decision.correlationId)
          pending.resolve(decision)
        }
      } catch (error) {
        const failure = error instanceof Error ? error : new Error(String(error))
        for (const pending of this.#pending.values()) pending.reject(failure)
        this.#pending.clear()
        this.#controlFailure?.(new RuntimeProtocolError("Actor control transport failed", { cause: failure }))
        this.stopControl()
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
  admission: (() => void) | undefined

  track<T>(operation: () => Promise<T>): Promise<T> {
    try {
      this.admission?.()
    } catch (error) {
      const rejected = Promise.reject<T>(error)
      void rejected.catch(() => {})
      return rejected
    }
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

  assertDrained(): void {
    if (this.#protocolFault !== undefined) throw this.#protocolFault
    if (this.#active !== 0) throw new RuntimeProtocolError("Run has runtime operations still pending")
  }
}

class ConsumingWaitGate {
  #pending = false

  get pending(): boolean { return this.#pending }

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
  event: programProto.RunEvent["event"],
): Promise<programProto.ResumeDecision> {
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

function requireWaitDecision(
  decision: programProto.ResumeDecision,
  correlationId: string,
  runWaitId: string,
  resumeAttachId: string,
  operation: string,
): void {
  if (
    decision.correlationId !== correlationId ||
    decision.runWaitId !== runWaitId ||
    decision.resumeAttachId !== resumeAttachId ||
    (decision.kind !== "completed" &&
      decision.kind !== "failed" &&
      decision.kind !== "cancelled")
  ) {
    throw new RuntimeProtocolError(
      `${operation} decision did not match the pending Wait`,
    )
  }
}

async function writeRuntimeProtocolEvent(
  io: ProgramIO,
  event: programProto.RunEvent["event"],
): Promise<void> {
  try {
    await writeRunEvent(io, event)
  } catch (error) {
    throw new RuntimeProtocolError("failed to write runtime protocol event", {
      cause: error,
    })
  }
}

async function acknowledgeResumeConsumed(
  io: ProgramIO,
  decision: programProto.ResumeDecision,
): Promise<void> {
  if (!decision.requireConsumedAck) return
  await writeRuntimeProtocolEvent(io, {
    case: "resumeConsumed",
    value: create(programProto.ResumeConsumedSchema, {
      runWaitId: decision.runWaitId,
      checkpointId: decision.checkpointId,
      resumeAttachId: decision.resumeAttachId,
      resumeRequestVersion: decision.resumeRequestVersion,
      runLeaseId: decision.runLeaseId,
      correlationId: decision.correlationId,
    }),
  })
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
  const start = fromBinary(programProto.ProgramStartSchema, await reader.read())
  validateProgramStart(start)

  const index = await loadProgramIndex(locatorURL, io)
  const kind = start.entrypoint.case
  if (kind !== "task" && kind !== "actor") {
    throw new Error("Program-start entrypoint is required")
  }
  const located = index.declarations.filter(
    (declaration) =>
      declaration.kind === kind &&
      declaration.declaredId === start.entrypointDeclaredId &&
      declaration.locator !== undefined,
  )
  if (located.length !== 1) {
    throw new Error(
      `Program declaration ${kind}:${JSON.stringify(start.entrypointDeclaredId)} was not found exactly once`,
    )
  }
  const declaration = located[0]!
  const locator = declaration.locator!
  const moduleURL = resolveModuleURL(locatorURL, locator.sourcePath)
  const imported = io.importModule === undefined
    ? await (await import("@helmr/module-execution")).importSourceExports(moduleURL)
    : await io.importModule(moduleURL)
  const definition = inspectDefinition(imported[locator.exportName])
  if (
    definition === undefined ||
    definition.kind !== declaration.kind ||
    definition.id !== declaration.declaredId ||
    (definition.kind !== "task" && definition.kind !== "actor")
  ) {
    throw new Error(
      `Program export ${JSON.stringify(locator.exportName)} does not match ${kind}:${JSON.stringify(start.entrypointDeclaredId)}`,
    )
  }
  validateEntrypointContract(start, definition)

  const identity = entrypointIdentity(kind, start.entrypointDeclaredId)
  await writeRunEvent(io, {
    case: "entrypointReady",
    value: create(programProto.EntrypointReadySchema, {
      runId: start.runId,
      attemptNumber: start.attemptNumber,
      entrypoint: identity,
    }),
  })

  const release = fromBinary(
    programProto.EntrypointReleaseSchema,
    await reader.read(),
  )
  validateEntrypointRelease(release, start, kind)
  const decisions = new ResumeDecisionRouter(reader)

  if (definition.kind === "task") {
    await runTask(start, definition, io, decisions)
  } else {
    await runActor(start, definition, io, decisions)
  }
  await reader.close()
}


async function loadProgramIndex(
  url: URL,
  io: ProgramIO,
): Promise<ProgramIndex> {
  const raw = io.readLocator === undefined
    ? await fs.readFile(url, "utf8")
    : await io.readLocator(url)
  const value: unknown = JSON.parse(raw)
  if (typeof value !== "object" || value === null) {
    throw new Error("Program index must be an object")
  }
  const record = value as Record<string, unknown>
  if (
    record["architecture"] !== "x86_64" ||
    record["runtimeContract"] !== "helmr.runtime.v0" ||
    typeof record["configResultDigest"] !== "string" ||
    !Array.isArray(record["queues"]) ||
    !Array.isArray(record["declarations"]) ||
    record["declarations"].length === 0
  ) {
    throw new Error("Program index has an invalid v0 shape")
  }
  const declarations = record["declarations"].map((entry, index) =>
    parseProgramIndexDeclaration(entry, index)
  )
  return { declarations }
}

function parseProgramIndexDeclaration(
  value: unknown,
  index: number,
): ProgramIndexDeclaration {
  if (typeof value !== "object" || value === null) {
    throw new Error(`Program index declaration ${index} must be an object`)
  }
  const record = value as Record<string, unknown>
  if (
    (
      record["kind"] !== "task" &&
      record["kind"] !== "actor" &&
      record["kind"] !== "sandbox"
    ) ||
    typeof record["declaredId"] !== "string" ||
    record["declaredId"] === "" ||
    typeof record["manifest"] !== "object" ||
    record["manifest"] === null
  ) {
    throw new Error(`Program index declaration ${index} is invalid`)
  }
  if (record["kind"] === "sandbox") {
    if (record["locator"] !== undefined) {
      throw new Error(`Program index Sandbox declaration ${index} has a locator`)
    }
    return {
      kind: "sandbox",
      declaredId: record["declaredId"],
    }
  }
  const locator = record["locator"]
  if (typeof locator !== "object" || locator === null) {
    throw new Error(`Program index declaration ${index} has no locator`)
  }
  const located = locator as Record<string, unknown>
  if (
    typeof located["exportName"] !== "string" ||
    located["exportName"] === "" ||
    typeof located["sourcePath"] !== "string" ||
    located["slot"] !== "handler"
  ) {
    throw new Error(`Program index declaration ${index} locator is invalid`)
  }
  return {
    kind: record["kind"],
    declaredId: record["declaredId"],
    locator: {
      exportName: located["exportName"],
      sourcePath: validateSourcePath(located["sourcePath"]),
      slot: "handler",
    },
  }
}

function validateSourcePath(value: string): string {
  const components = value.split("/")
  if (components.some((part) => part === "" || part === "." || part === ".." || part === "node_modules" || part.includes("\\") || /[\u0000-\u001f\u007f-\u009f]/.test(part)) ||
      components[0] === "helmr" || value === "helmr.config.ts" ||
      !/\.(?:[cm]?js|jsx|[cm]?ts|tsx)$/.test(value) || /\.d\.[cm]?ts$/.test(value)) {
    throw new Error("declaration sourcePath must identify a project source module")
  }
  return value
}

function resolveModuleURL(locatorURL: URL, sourcePath: string): URL {
  const root = path.dirname(path.dirname(fileURLToPath(locatorURL)))
  const resolved = path.resolve(root, sourcePath)
  const relative = path.relative(root, resolved)
  if (
    relative === "" ||
    relative === ".." ||
    relative.startsWith(`..${path.sep}`) ||
    path.isAbsolute(relative)
  ) {
    throw new Error("declaration sourcePath escapes the Program root")
  }
  return pathToFileURL(resolved)
}

function validateProgramStart(start: programProto.ProgramStart): void {
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
  start: programProto.ProgramStart,
  definition: InternalTaskDefinition | InternalActorDefinition,
): void {
  if (definition.kind === "actor") {
    if (
      start.entrypoint.case !== "actor" ||
      start.entrypoint.value.startInputSequence < 0n ||
      start.entrypoint.value.runGeneration <= 0n ||
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
): programProto.EntrypointIdentity {
  return create(programProto.EntrypointIdentitySchema, {
    declaredId,
    kind: kind === "task"
      ? {
          case: "task",
          value: create(programProto.TaskEntrypointSchema),
        }
      : {
          case: "actor",
          value: create(programProto.ActorEntrypointSchema),
        },
  })
}

function validateEntrypointRelease(
  release: programProto.EntrypointRelease,
  start: programProto.ProgramStart,
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
  start: programProto.ProgramStart,
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
    programRuntimeOperations(
      start,
      io,
      decisions,
      new ConsumingWaitGate(),
      runOperations,
    ),
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
    value: create(programProto.TaskOutcomeSchema, {
      outcome: {
        case: "succeeded",
        value: create(programProto.TaskSucceededSchema, {
          outputJson: new TextDecoder().decode(normalized),
        }),
      },
    }),
  })
}

function programRuntimeOperations(
  start: programProto.ProgramStart,
  io: ProgramIO,
  decisions: ResumeDecisionRouter,
  waitGate: ConsumingWaitGate,
  runOperations: RunOperationState,
  actor?: ActorRuntime,
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
    const correlationId = newUUIDv7()
    const payloadJson = target.payloadPresent
      ? new TextDecoder().decode(canonicalizeJsonValue(payload as JsonValue))
      : undefined
    const workspaceJson = new TextDecoder().decode(
      canonicalizeJsonValue({ id: workspaceRefID(options.workspace) }),
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
        value: create(programProto.TaskChildInvokeRequestedSchema, {
          correlationId,
          declaredId: target.declaredId,
          method: "start",
          ...(actor === undefined ? {} : { actorSpeculativeInputSequence: actor.cursor.value, execution: actor.execution, ...(actor.active === undefined ? {} : { turnId: actor.active.scope.turnId }) }),
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
        const id = resourceID(
          (value as Record<string, unknown>)["run_id"],
          "Task child start result.run_id",
        )
        return createRunHandle(id)
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
        const correlationId = newUUIDv7()
        const runWaitId = newUUIDv7()
        const resumeAttachId = newUUIDv7()
        const payloadJson = target.payloadPresent
          ? new TextDecoder().decode(
              canonicalizeJsonValue(payload as JsonValue),
            )
          : undefined
        const workspaceJson = new TextDecoder().decode(
          canonicalizeJsonValue({ id: workspaceRefID(options.workspace) }),
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
            value: create(programProto.TaskChildInvokeRequestedSchema, {
              correlationId,
              runWaitId,
              resumeAttachId,
              declaredId: target.declaredId,
              method: "call",
              payloadPresent: target.payloadPresent,
              ...(payloadJson === undefined ? {} : { payloadJson }),
              workspaceJson,
              optionsJson: new TextDecoder().decode(
                canonicalizeJsonValue(requestOptions),
              ),
              idempotencyKey: options.idempotencyKey,
              ...(actor === undefined
                ? {}
                : { actorSpeculativeInputSequence: actor.cursor.value, execution: actor.execution, ...(actor.active === undefined ? {} : { turnId: actor.active.scope.turnId }) }),
            }),
          },
        )
        requireWaitDecision(
          decision,
          correlationId,
          runWaitId,
          resumeAttachId,
          "Task child call",
        )
        if (runOperations.controller.signal.aborted) throw runOperations.controller.signal.reason
      await acknowledgeResumeConsumed(io, decision)
      if (runOperations.controller.signal.aborted) throw runOperations.controller.signal.reason

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
        try { await actor?.resumeMessageReady() } finally { releaseWait() }
      }
    })
    return abortableRuntimeOperation(operation, options.signal)
  }
  const performWait = async (
    params: JsonValue,
    timeoutMs: number,
  ): Promise<void> => {
    const releaseWait = waitGate.acquire()
    const correlationId = newUUIDv7()
    const runWaitId = newUUIDv7()
    const resumeAttachId = newUUIDv7()
    try {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "runWaitRequested",
        value: create(programProto.RunWaitRequestedSchema, {
          correlationId,
          runWaitId,
          resumeAttachId,
          kind: "timer",
          paramsJson: new TextDecoder().decode(canonicalizeJsonValue(params)),
          timeoutMs: BigInt(timeoutMs),
          ...(actor === undefined
            ? {}
            : { actorSpeculativeInputSequence: actor.cursor.value, execution: actor.execution, ...(actor.active === undefined ? {} : { turnId: actor.active.scope.turnId }) }),
        }),
      })
      requireWaitDecision(
        decision,
        correlationId,
        runWaitId,
        resumeAttachId,
        "timer resume",
      )
      if (runOperations.controller.signal.aborted) throw runOperations.controller.signal.reason
      await acknowledgeResumeConsumed(io, decision)
      if (runOperations.controller.signal.aborted) throw runOperations.controller.signal.reason

      if (decision.kind !== "completed") {
        const failure = parseRuntimeProtocolValue(
          "timer Wait failure decision",
          () => resumeFailure(decision.dataJson),
        )
        throw new RuntimeProtocolError(`timer Wait ${decision.kind}: ${failure.reasonCode}`)
      }
    } finally {
      try { await actor?.resumeMessageReady() } finally { releaseWait() }
    }
  }
  const wait = (params: JsonValue, timeoutMs: number): Promise<void> =>
    runOperations.track(() => performWait(params, timeoutMs))
  const performActorStart = async (
    declaredId: string,
    options: ActorStartOptions,
  ): Promise<Readonly<{ sessionId: string; runId: string }>> => {
    if (options.signal?.aborted) throw abortSignalReason(options.signal)
    const correlationId = newUUIDv7()
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
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "actorStartRequested",
        value: create(programProto.ActorStartRequestedSchema, {
          correlationId,
          declaredId,
          workspaceId: workspaceRefID(options.workspace),
          ...(options.key === undefined ? {} : { key: options.key }),
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
        requireExactKeys(value, ["run_id", "session_id"], "Actor start result")
        const sessionId = resourceID(
          stringField(value, "session_id", "Actor start result"),
          "Actor start result.session_id",
        )
        const runId = resourceID(
          stringField(value, "run_id", "Actor start result"),
          "Actor start result.run_id",
        )
        return Object.freeze({ sessionId, runId })
      })
    })
    return abortableRuntimeOperation(operation, options.signal)
  }
  const performSessionStatus = async (
    sessionId: string,
    signal?: AbortSignal,
  ): Promise<Session> => {
    if (signal?.aborted) throw abortSignalReason(signal)
    const correlationId = newUUIDv7()
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "sessionStatusRequested",
        value: create(programProto.SessionStatusRequestedSchema, {
          correlationId, sessionId,
        }),
      })
      requireRuntimeOperationDecision(decision, correlationId, "Session retrieve")
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Session retrieve", decision.dataJson)
      }
      return parseRuntimeProtocolValue(
        "Session retrieve result",
        () => parseSession(JSON.parse(decision.dataJson)),
      )
    })
    return abortableRuntimeOperation(operation, signal)
  }
  const sessionOperation = async <T>(
    event: (correlationId: string) => programProto.RunEvent["event"],
    parse: (value: unknown) => T,
    signal?: AbortSignal,
  ): Promise<T> => {
    if (signal?.aborted) throw abortSignalReason(signal)
    const correlationId = newUUIDv7()
    const pending = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, event(correlationId))
      requireRuntimeOperationDecision(decision, correlationId, "Session operation")
      if (decision.kind === "failed") throw runtimeOperationFailure("Session operation", decision.dataJson)
      return parseRuntimeProtocolValue("Session operation", () => parse(JSON.parse(decision.dataJson)))
    })
    return abortableRuntimeOperation(pending, signal)
  }
  const submitSession = (mode: "send" | "enqueue" | "message", sessionId: string, data: JsonValue, turnId: string | undefined, request?: SessionOperationOptions, signal?: AbortSignal) => {
    const dataJson = jsonText(data)
    if (Buffer.byteLength(dataJson) > MAX_ACTOR_INPUT_BYTES) return Promise.reject(new Error("Session data exceeds maximum size"))
    return sessionOperation(correlationId => ({ case: "sessionSubmitRequested", value: create(programProto.SessionSubmitRequestedSchema, {
      correlationId, sessionId, mode, dataJson, ...(turnId === undefined ? {} : { turnId }), idempotencyKey: request?.idempotencyKey ?? newUUIDv7(),
    }) }), (value): import("@helmr/sdk").SessionMessageReceipt | import("@helmr/sdk").SessionAdmissionReceipt => mode === "message" ? parseSessionMessageReceipt(value) : parseSessionAdmissionReceipt(value), signal)
  }
  const workspaceAddress = (workspaceId: string) =>
    create(programProto.WorkspaceAddressSchema, { workspaceId })
  const performWorkspaceCreate = async (
    declaredId: string,
    request: WorkspaceCreateRequest = {},
    signal?: AbortSignal,
  ): Promise<Readonly<{ workspaceId: string }>> => {
    if (signal?.aborted) throw abortSignalReason(signal)
    const correlationId = newUUIDv7()
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "workspaceCreateRequested",
        value: create(programProto.WorkspaceCreateRequestedSchema, {
          correlationId,
          declaredId,
          ...(request.key === undefined ? {} : { key: request.key }),
          secrets: encodeWorkspaceSecrets(request.secrets).map((secret) =>
            create(programProto.WorkspaceSecretPlacementSchema, {
              secret: secret.secret,
              placement: secret.env !== undefined
                ? { case: "env", value: create(programProto.SecretEnvBindingSchema, { name: secret.env.name, mode: secret.env.mode, allowedOrigins: [...(secret.env.allowed_origins ?? [])] }) }
                : { case: "file", value: create(programProto.SecretFileBindingSchema, { path: secret.file.path }) },
            })
          ) ?? [],
          ...(request.idempotencyKey === undefined
            ? {}
            : { idempotencyKey: request.idempotencyKey }),
        }),
      })
      requireRuntimeOperationDecision(decision, correlationId, "Workspace create")
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Workspace create", decision.dataJson)
      }
      return parseRuntimeProtocolValue("Workspace create result", () => {
        const value = parseObjectJSON(decision.dataJson, "Workspace create result")
        requireExactKeys(value, ["workspace_id"], "Workspace create result")
        const workspaceId = resourceID(
          stringField(
            value,
            "workspace_id",
            "Workspace create result",
          ),
          "Workspace create result.workspace_id",
        )
        return Object.freeze({ workspaceId })
      })
    })
    return abortableRuntimeOperation(operation, signal)
  }
  const performWorkspaceRetrieve = async (
    workspaceId: string,
    signal?: AbortSignal,
  ): Promise<Workspace> => {
    if (signal?.aborted) throw abortSignalReason(signal)
    const correlationId = newUUIDv7()
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "workspaceRetrieveRequested",
        value: create(programProto.WorkspaceRetrieveRequestedSchema, {
          correlationId,
          workspace: workspaceAddress(workspaceId),
        }),
      })
      requireRuntimeOperationDecision(decision, correlationId, "Workspace retrieve")
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Workspace retrieve", decision.dataJson)
      }
      return parseRuntimeProtocolValue(
        "Workspace retrieve result",
        () => parseWorkspace(JSON.parse(decision.dataJson)),
      )
    })
    return abortableRuntimeOperation(operation, signal)
  }
  const performWorkspaceExec = async (
    workspaceId: string,
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
    const correlationId = newUUIDv7()
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "workspaceExecRequested",
        value: create(programProto.WorkspaceExecRequestedSchema, {
          correlationId,
          workspace: workspaceAddress(workspaceId),
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
    workspaceId: string,
    request: WorkspaceDeleteRequest = {},
    signal?: AbortSignal,
  ): Promise<WorkspaceDeleteReceipt> => {
    if (signal?.aborted) throw abortSignalReason(signal)
    const correlationId = newUUIDv7()
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "workspaceDeleteRequested",
        value: create(programProto.WorkspaceDeleteRequestedSchema, {
          correlationId,
          workspace: workspaceAddress(workspaceId),
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
    request: TokenCreateRequest,
  ): Promise<TokenCreateResult> => {
    const correlationId = newUUIDv7()
    const timeoutMs = request.timeout === undefined
      ? undefined
      : durationMilliseconds(request.timeout, "Token timeout")
    const metadataJson = request.metadata === undefined
      ? undefined
      : new TextDecoder().decode(canonicalizeJsonValue(request.metadata))
    const idempotencyKey = normalizeTokenIdempotencyKey(request.idempotencyKey)
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "tokenCreateRequested",
        value: create(programProto.TokenCreateRequestedSchema, {
          correlationId,
          ...(timeoutMs === undefined ? {} : { timeoutMs: BigInt(timeoutMs) }),
          ...(idempotencyKey === undefined ? {} : { idempotencyKey }),
          tags: request.tags === undefined ? [] : [...request.tags],
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
    const correlationId = newUUIDv7()
    const runWaitId = newUUIDv7()
    const resumeAttachId = newUUIDv7()
    const timeoutMs = options.timeout === undefined
      ? undefined
      : durationMilliseconds(options.timeout, "Token Wait timeout")
    const idleTimeoutMs = options.idleTimeout === undefined
      ? undefined
      : tokenWaitIdleTimeoutMilliseconds(options.idleTimeout)
    try {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "runWaitRequested",
        value: create(programProto.RunWaitRequestedSchema, {
          correlationId,
          runWaitId,
          resumeAttachId,
          kind: "token",
          paramsJson: JSON.stringify({ token_id: tokenId }),
          ...(options.metadata === undefined
            ? {}
            : { metadataJson: new TextDecoder().decode(canonicalizeJsonValue(options.metadata)) }),
          ...(timeoutMs === undefined ? {} : { timeoutMs: BigInt(timeoutMs) }),
          ...(idleTimeoutMs === undefined ? {} : { idleTimeoutMs: BigInt(idleTimeoutMs) }),
          tags: options.tags === undefined ? [] : [...options.tags],
          ...(actor === undefined
            ? {}
            : { actorSpeculativeInputSequence: actor.cursor.value, execution: actor.execution, ...(actor.active === undefined ? {} : { turnId: actor.active.scope.turnId }) }),
        }),
      })
      requireWaitDecision(
        decision,
        correlationId,
        runWaitId,
        resumeAttachId,
        "Token resume",
      )
      if (runOperations.controller.signal.aborted) throw runOperations.controller.signal.reason
      await acknowledgeResumeConsumed(io, decision)
      if (runOperations.controller.signal.aborted) throw runOperations.controller.signal.reason

      if (decision.kind !== "completed") {
        if (decision.kind !== "failed" && decision.kind !== "cancelled") {
          throw new RuntimeProtocolError("Token resume decision kind was invalid")
        }
        throw tokenWaitFailure(decision.kind, decision.dataJson)
      }
      return parseRuntimeProtocolValue(
        "Token completion result",
        () => JSON.parse(decision.dataJson) as JsonValue,
      )
    } finally {
      try { await actor?.resumeMessageReady() } finally { releaseWait() }
    }
  }
  const performMetadataMutation = async (
    request:
      | Readonly<{ operation: "set"; key: string; value: JsonValue }>
      | Readonly<{ operation: "patch"; values: Metadata }>
      | Readonly<{ operation: "increment"; key: string; amount: number }>,
  ): Promise<void> => {
    const correlationId = newUUIDv7()
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "metadataUpdated",
        value: create(programProto.MetadataUpdatedSchema, {
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
    if (Buffer.byteLength(message) > MAX_RUN_LOG_MESSAGE_BYTES) {
      throw new Error(
        `logger message must be at most ${MAX_RUN_LOG_MESSAGE_BYTES} UTF-8 bytes`,
      )
    }
    const attributesJson = canonicalizeLogAttributes(attributes)
    const correlationId = newUUIDv7()
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "structuredLogRequested",
        value: create(programProto.StructuredLogRequestedSchema, {
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
    actorStart: performActorStart,
    sessionRetrieve: performSessionStatus,
    async sessionSend(sessionId, data, request, signal) {
      return await submitSession("send", sessionId, data, undefined, request, signal) as import("@helmr/sdk").SessionAdmissionReceipt
    },
    async sessionEnqueue(sessionId, data, request, signal) {
      const result = await submitSession("enqueue", sessionId, data, undefined, request, signal)
      if (!("kind" in result) || result.kind !== "enqueued") throw new RuntimeProtocolError("Enqueue returned a message receipt")
      return result
    },
    async sessionTurnSend(sessionId, turnId, data, request, signal) {
      return await submitSession("message", sessionId, data, turnId, request, signal) as import("@helmr/sdk").SessionMessageReceipt
    },
    sessionTurnRetrieve(sessionId, turnId, signal) {
      return sessionOperation(correlationId => ({ case: "sessionTurnRetrieveRequested", value: create(programProto.SessionTurnRetrieveRequestedSchema, { correlationId, sessionId, turnId }) }), parseTurnState, signal)
    },
    sessionTurnInterrupt(sessionId, turnId, request, signal) {
      return sessionOperation(correlationId => ({ case: "sessionTurnInterruptRequested", value: create(programProto.SessionTurnInterruptRequestedSchema, { correlationId, sessionId, turnId, idempotencyKey: request?.idempotencyKey ?? newUUIDv7() }) }), parseTurnInterruptReceipt, signal)
    },
    sessionEvents(sessionId, query, signal) {
      return sessionOperation(correlationId => ({ case: "sessionEventsRequested", value: create(programProto.SessionEventsRequestedSchema, { correlationId, sessionId, after: BigInt(query?.after ?? 0), limit: query?.limit ?? 100 }) }), parseSessionEventPage, signal)
    },
    sessionClose(sessionId, request, signal) {
      return sessionOperation(correlationId => ({ case: "sessionCloseRequested", value: create(programProto.SessionCloseRequestedSchema, { correlationId, sessionId, idempotencyKey: request?.idempotencyKey ?? newUUIDv7() }) }), parseSessionCloseReceipt, signal)
    },
    sessionCancel(sessionId, request, signal) {
      return sessionOperation(correlationId => ({ case: "sessionCancelRequested", value: create(programProto.SessionCancelRequestedSchema, { correlationId, sessionId, idempotencyKey: request?.idempotencyKey ?? newUUIDv7() }) }), parseSessionCancelReceipt, signal)
    },
    sessionResume(sessionId, request, signal) {
      return sessionOperation(correlationId => ({ case: "sessionResumeRequested", value: create(programProto.SessionResumeRequestedSchema, { correlationId, sessionId, holdId: request.holdId, idempotencyKey: request.idempotencyKey ?? newUUIDv7() }) }), parseSessionResumeReceipt, signal)
    },
    workspaceCreate(declaredId, request, signal) {
      return performWorkspaceCreate(declaredId, request, signal)
    },
    workspaceRetrieve(address, signal) {
      return performWorkspaceRetrieve(address, signal)
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
  if (Buffer.byteLength(value) > 512) {
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
  if (Buffer.byteLength(normalized) > 512) {
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
    id: resourceID(
      stringField(value, "id", "Token create result"),
      "Token create result.id",
    ),
    callbackUrl: stringField(value, "callback_url", "Token create result"),
    publicAccessToken: stringField(value, "public_access_token", "Token create result"),
    timeoutAt: timestampString(value["timeout_at"], "Token create result.timeout_at"),
    status: "pending" as const,
    metadata,
    tags: Object.freeze([...tags]) as readonly string[],
    createdAt: timestampString(value["created_at"], "Token create result.created_at"),
    updatedAt: timestampString(value["updated_at"], "Token create result.updated_at"),
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
  const error = new Error(message) as Error & { code: string }
  error.name = "HelmrError"
  error.code = code
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
    ["failure", "ok", "run"],
    "Task child call failure",
  )
  const rawFailure = objectField(value, "failure", "Task child call failure")
  requireExactKeys(
    rawFailure,
    ["code", "details", "message"],
    "Task child call failure.failure",
  )
  const details = objectField(
    rawFailure,
    "details",
    "Task child call failure.failure",
  )
  const failure = Object.freeze({
    code: stringField(
      rawFailure,
      "code",
      "Task child call failure.failure",
    ),
    message: stringField(rawFailure, "message", "Task child call failure.failure"),
    details: Object.freeze({ ...details }) as Readonly<Record<string, JsonValue>>,
  })
  return Object.freeze({
    ok: false,
    failure,
    run: parseTaskResultRun(value),
  })
}

function parseTaskResultRun(
  value: Record<string, unknown>,
): import("@helmr/sdk").RunHandle<JsonValue> {
  const run = objectField(value, "run", "Task child call result")
  requireExactKeys(run, ["id"], "Task child call result.run")
  const id = resourceID(
    stringField(run, "id", "Task child call result.run"),
    "Task child call result.run.id",
  )
  return createRunHandle(id)
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
  ) as Error & { code: string }
  error.name = code === "wait_timeout" ? "WaitTimeoutError" : "HelmrError"
  error.code = code
  return error
}

function requireRuntimeOperationDecision(
  decision: programProto.ResumeDecision,
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

async function abortableRuntimeOperation<T>(
  operation: Promise<T>,
  signal: AbortSignal | undefined,
): Promise<T> {
  if (signal === undefined) return operation
  if (signal.aborted) throw abortSignalReason(signal)
  const aborted = Promise.withResolvers<never>()
  const onAbort = () => aborted.reject(abortSignalReason(signal))
  signal.addEventListener("abort", onAbort, { once: true })
  try {
    return await Promise.race([operation, aborted.promise])
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
  const value = parseObjectJSON(dataJson, "terminal Wait failure data")
  return {
    reasonCode: stringField(
      value,
      "reason_code",
      "terminal Wait failure data",
    ),
  }
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

interface TurnRuntime {
  readonly scope: programProto.TurnExecution
  readonly sequence: bigint
  readonly controller: AbortController
  phase: "running" | "settling" | "settled"
  ready?: Promise<void> | undefined
  claim?: Promise<void> | undefined
  callback?: Promise<void> | undefined
  handler?: (message: Message) => unknown
}

interface MessageCallback {
  readonly turn: TurnRuntime
  readonly deliveryId: string
  readonly writes: Set<Promise<unknown>>
  error?: unknown
}

const messageCallback = new AsyncLocalStorage<MessageCallback>()

class ActorRuntime {
  readonly execution: programProto.SessionExecution
  readonly cursor: { value: bigint }
  readonly #mainWrites = new Set<Promise<unknown>>()
  #outputError: unknown
  #uncertain: unknown
  #settlement: Promise<void> | undefined
  #receiveCorrelation: string | undefined
  #pendingSettlementTurn: TurnRuntime | undefined
  active: TurnRuntime | undefined
  stop: { holdId: string; turnId?: string } | undefined

  readonly start: programProto.ProgramStart
  readonly definition: InternalActorDefinition
  readonly io: ProgramIO
  readonly decisions: ResumeDecisionRouter
  readonly waitGate: ConsumingWaitGate
  readonly operations: RunOperationState

  constructor(
    start: programProto.ProgramStart,
    definition: InternalActorDefinition,
    io: ProgramIO,
    decisions: ResumeDecisionRouter,
    waitGate: ConsumingWaitGate,
    operations: RunOperationState,
  ) {
    this.start = start
    this.definition = definition
    this.io = io
    this.decisions = decisions
    this.waitGate = waitGate
    this.operations = operations
    if (start.entrypoint.case !== "actor")
      throw new Error("Actor start required")
    this.cursor = { value: start.entrypoint.value.startInputSequence }
    this.execution = create(programProto.SessionExecutionSchema, {
      sessionId: start.entrypoint.value.sessionId,
      runId: start.runId,
      attemptNumber: start.attemptNumber,
      runGeneration: start.entrypoint.value.runGeneration,
    })
  }

  assertAdmission(): void {
    if (this.#uncertain !== undefined) throw this.#uncertain
    const callback = messageCallback.getStore()
    if (
      callback !== undefined &&
      (callback.turn !== this.active || callback.turn.phase === "settled")
    )
      throw new Error("Message callback no longer owns its Turn")
    if (this.operations.controller.signal.aborted)
      throw this.operations.controller.signal.reason
    if (
      this.active?.phase === "settling" &&
      messageCallback.getStore()?.turn !== this.active
    ) {
      throw new Error("Turn settlement has started")
    }
  }

  control(decision: programProto.ResumeDecision): void {
    const value = parseRuntimeProtocolValue("Session stop", () =>
      parseObjectJSON(decision.dataJson, "Session stop"),
    )
    const execution = objectField(value, "execution", "Session stop")
    if (
      execution["session_id"] !== this.execution.sessionId ||
      execution["run_id"] !== this.execution.runId ||
      execution["attempt_number"] !== this.execution.attemptNumber ||
      execution["run_generation"] !== Number(this.execution.runGeneration)
    ) {
      throw new RuntimeProtocolError(
        "Session stop does not match current execution",
      )
    }
    const turnId = value["turn_id"]
    const pendingReceive =
      this.active === undefined && this.#receiveCorrelation !== undefined
    const pendingSettlement =
      turnId === null &&
      this.active !== undefined &&
      this.#pendingSettlementTurn === this.active
    if (
      turnId !== (this.active?.scope.turnId ?? null) &&
      !(pendingReceive && typeof turnId === "string") &&
      !pendingSettlement
    ) {
      throw new RuntimeProtocolError("Session stop does not match current Turn")
    }
    const holdId = resourceID(value["hold_id"], "Session stop.hold_id")
    if (
      this.stop !== undefined &&
      (this.stop.holdId !== holdId || (this.stop.turnId ?? null) !== turnId)
    )
      throw new RuntimeProtocolError("Session stop binding changed")
    this.stop = {
      holdId,
      ...(turnId === null
        ? {}
        : { turnId: resourceID(turnId, "Session stop.turn_id") }),
    }
    const error = this.operations.cancel(
      stringField(value, "reason", "Session stop"),
    )
    this.active?.controller.abort(error)
  }

  uncertain(error: unknown): void {
    this.#uncertain ??= error
    this.operations.controller.abort(error)
    this.active?.controller.abort(error)
  }

  async request(
    event: programProto.RunEvent["event"],
    correlationId: string,
    label: string,
  ): Promise<programProto.ResumeDecision> {
    const decision = await requestRuntimeDecision(
      this.io,
      this.decisions,
      correlationId,
      event,
    )
    requireRuntimeOperationDecision(decision, correlationId, label)
    if (decision.kind === "failed")
      throw runtimeOperationFailure(label, decision.dataJson)
    return decision
  }

  async receive(options?: ActorSessionReceiveOptions): Promise<Turn | null> {
    this.assertAdmission()
    if (this.active !== undefined)
      throw new Error("Current Turn must be explicitly settled before receive")
    const release = this.waitGate.acquire()
    const correlationId = newUUIDv7()
    try {
      const runWaitId = newUUIDv7(),
        resumeAttachId = newUUIDv7()
      this.#receiveCorrelation = correlationId
      const decision = await this.operations.track(() =>
        requestRuntimeDecision(this.io, this.decisions, correlationId, {
          case: "runWaitRequested",
          value: create(programProto.RunWaitRequestedSchema, {
            correlationId,
            runWaitId,
            resumeAttachId,
            kind: "actor_input",
            execution: this.execution,
            paramsJson: JSON.stringify({
              session_id: this.execution.sessionId,
              after_input_sequence: Number(this.cursor.value),
            }),
            actorSpeculativeInputSequence: this.cursor.value,
            ...(options?.timeout === undefined
              ? {}
              : { timeoutMs: BigInt(durationMilliseconds(options.timeout)) }),
            ...(options?.idleTimeout === undefined
              ? {}
              : {
                  idleTimeoutMs: BigInt(
                    durationMilliseconds(options.idleTimeout),
                  ),
                }),
            ...(options?.metadata === undefined
              ? {}
              : { metadataJson: jsonText(options.metadata) }),
            tags: options?.tags === undefined ? [] : [...options.tags],
          }),
        }),
      )
      requireWaitDecision(
        decision,
        correlationId,
        runWaitId,
        resumeAttachId,
        "Session receive",
      )
      if (this.operations.controller.signal.aborted)
        throw this.operations.controller.signal.reason
      await acknowledgeResumeConsumed(this.io, decision)
      if (this.operations.controller.signal.aborted)
        throw this.operations.controller.signal.reason
      if (decision.kind !== "completed") {
        const failure = resumeFailure(decision.dataJson)
        if (failure.reasonCode === "session_closed") return null
        if (decision.kind === "cancelled")
          throw new Error(`Session receive cancelled: ${failure.reasonCode}`)
        throw Object.assign(
          new Error(`Session receive failed: ${failure.reasonCode}`),
          { code: failure.reasonCode },
        )
      }
      const data = parseRuntimeProtocolValue("Turn delivery", () =>
        parseObjectJSON(decision.dataJson, "Turn delivery"),
      )
      const turn = objectField(data, "turn", "Turn delivery")
      const sequence = safeJSONSequence(turn["sequence"], "Turn sequence")
      if (
        BigInt(sequence) !== this.cursor.value + 1n ||
        data["run_generation"] !== Number(this.execution.runGeneration)
      ) {
        throw new RuntimeProtocolError(
          "Turn delivery does not match execution frontier",
        )
      }
      const state: TurnRuntime = {
        scope: create(programProto.TurnExecutionSchema, {
          session: this.execution,
          turnId: resourceID(turn["id"], "Turn id"),
        }),
        sequence: BigInt(sequence),
        controller: new AbortController(),
        phase: "running",
      }
      this.active = state
      this.cursor.value = state.sequence
      if (this.operations.controller.signal.aborted)
        throw this.operations.controller.signal.reason
      const source = objectField(turn, "source", "Turn source")
      let parsedSource: TurnSource
      if (source["type"] === "external") parsedSource = { type: "external" }
      else if (source["type"] === "run")
        parsedSource = {
          type: "run",
          runId: resourceID(source["run_id"], "Turn source Run"),
        }
      else throw new RuntimeProtocolError("Invalid Turn source")
      return Object.freeze({
        id: state.scope.turnId,
        sequence,
        input: data["value"] as JsonValue,
        source: Object.freeze(parsedSource),
        createdAt: timestampString(turn["created_at"], "Turn created_at"),
        signal: state.controller.signal,
        output: this.writer(state),
        onMessage: (handler: (message: Message) => unknown) =>
          this.onMessage(state, handler),
        complete: (...result: [JsonValue?]) =>
          this.settle(
            state,
            "completed",
            result[0],
            result.length !== 0 && result[0] !== undefined,
          ),
        fail: (error: unknown) =>
          this.settle(state, "failed", failureJSON(error)),
      })
    } catch (error) {
      if (error instanceof RuntimeProtocolError) this.uncertain(error)
      throw error
    } finally {
      if (this.#receiveCorrelation === correlationId)
        this.#receiveCorrelation = undefined
      release()
    }
  }

  onMessage(
    turn: TurnRuntime,
    handler: (message: Message) => unknown,
  ): Promise<void> {
    this.assertTurn(turn)
    if (turn.handler !== undefined)
      return Promise.reject(
        new Error("Turn message handler is already installed"),
      )
    if (typeof handler !== "function")
      return Promise.reject(
        new Error("Turn message handler must be a function"),
      )
    turn.handler = handler
    const correlationId = newUUIDv7()
    turn.ready = this.operations.trackDrainable(async () => {
      await this.request(
        {
          case: "turnReadyRequested",
          value: create(programProto.TurnReadyRequestedSchema, {
            correlationId,
            execution: turn.scope,
          }),
        },
        correlationId,
        "Turn readiness",
      )
      void this.messageLoop(turn).catch((error) => {
        if (!this.stoppedRejection(error)) this.uncertain(error)
      })
    })
    return turn.ready
  }

  async resumeMessageReady(): Promise<void> {
    const turn = this.active
    if (
      turn?.handler === undefined ||
      turn.phase !== "running" ||
      this.operations.controller.signal.aborted
    )
      return
    const correlationId = newUUIDv7()
    turn.ready = this.operations.trackDrainable(async () => {
      await this.request(
        {
          case: "turnReadyRequested",
          value: create(programProto.TurnReadyRequestedSchema, {
            correlationId,
            execution: turn.scope,
          }),
        },
        correlationId,
        "Turn readiness after wait",
      )
    })
    await turn.ready
  }

  async messageLoop(turn: TurnRuntime): Promise<void> {
    while (
      turn.phase === "running" &&
      !this.operations.controller.signal.aborted
    ) {
      if (this.waitGate.pending) {
        await new Promise((resolve) => setTimeout(resolve, 25))
        continue
      }
      const correlationId = newUUIDv7(),
        deliveryId = newUUIDv7()
      turn.claim = (async () => {
        const response = await this.request(
          {
            case: "turnMessageClaimRequested",
            value: create(programProto.TurnMessageClaimRequestedSchema, {
              correlationId,
              execution: turn.scope,
              deliveryId,
            }),
          },
          correlationId,
          "Turn message claim",
        )
        const body = parseRuntimeProtocolValue("Turn message delivery", () =>
          parseObjectJSON(response.dataJson, "Turn message delivery"),
        )
        if (body["delivery"] === null) return
        const delivery = objectField(body, "delivery", "Turn message delivery")
        if (
          delivery["turn_id"] !== turn.scope.turnId ||
          delivery["delivery_id"] !== deliveryId
        )
          throw new RuntimeProtocolError(
            "Message delivery does not match requested Turn",
          )
        turn.callback = this.handleMessage(
          turn,
          deliveryId,
          resourceID(delivery["message_id"], "Message id"),
          delivery["data"],
        )
      })()
      try {
        await turn.claim
      } catch (error) {
        if ((error as { code?: string }).code !== "turn_not_ready") throw error
      }
      turn.claim = undefined
      if (turn.callback !== undefined) {
        await turn.callback
        turn.callback = undefined
      } else if (turn.phase === "running")
        await new Promise((resolve) => setTimeout(resolve, 25))
    }
  }

  async handleMessage(
    turn: TurnRuntime,
    deliveryId: string,
    messageId: string,
    data: unknown,
  ): Promise<void> {
    const context: MessageCallback = { turn, deliveryId, writes: new Set() }
    let status = "handled",
      code = "",
      details: JsonValue | undefined
    try {
      await messageCallback.run(context, async () => {
        await turn.handler!({ id: messageId, data: data as JsonValue })
      })
      await drainPromises(context.writes)
      if (context.error !== undefined) throw context.error
    } catch (error) {
      await drainPromises(context.writes)
      if (error instanceof MessageRejected && context.error === undefined) {
        status = "rejected"
        code = "handler_rejected"
        details = error.details
      } else {
        status = "unknown"
        code = "handler_failed"
        details = failureJSON(error)
        this.#uncertain ??= new RuntimeProtocolError(
          "Message handler outcome is unknown",
          { cause: error },
        )
      }
    }
    const correlationId = newUUIDv7()
    await this.request(
      {
        case: "turnMessageCompleteRequested",
        value: create(programProto.TurnMessageCompleteRequestedSchema, {
          correlationId,
          execution: turn.scope,
          messageId,
          deliveryId,
          status,
          code,
          ...(details === undefined ? {} : { detailsJson: jsonText(details) }),
        }),
      },
      correlationId,
      "Turn message completion",
    )
    if (this.#uncertain !== undefined) this.uncertain(this.#uncertain)
  }

  assertTurn(turn: TurnRuntime): void {
    this.assertAdmission()
    if (this.active !== turn || turn.phase !== "running")
      throw new Error("Turn is not writable")
  }

  writer(turn?: TurnRuntime): RecordWriter {
    const track = <T>(
      operation: (callback?: MessageCallback) => Promise<T>,
    ): Promise<T> => {
      const callback = messageCallback.getStore()
      try {
        this.assertAdmission()
        if (
          turn !== undefined &&
          (this.active !== turn ||
            (turn.phase !== "running" &&
              !(turn.phase === "settling" && callback?.turn === turn)))
        )
          throw new Error("Turn is not writable")
      } catch (error) {
        const rejected = Promise.reject<T>(error)
        void rejected.catch(() => {})
        return rejected
      }
      const writes = callback?.writes ?? this.#mainWrites
      const pending = this.operations.trackDrainable(async () => {
        try {
          return await operation(callback)
        } catch (error) {
          const failure =
            error ?? new Error("Actor output failed", { cause: error })
          if (callback !== undefined) callback.error ??= failure
          else this.#outputError ??= failure
          throw failure
        }
      })
      writes.add(pending)
      void pending.finally(() => writes.delete(pending)).catch(() => {})
      return pending
    }
    const write = async (
      value: JsonValue,
      options?: SessionOperationOptions,
      callback?: MessageCallback,
    ): Promise<OutputReceipt> => {
      await turn?.ready
      const correlationId = newUUIDv7()
      const common = {
        correlationId,
        dataJson: jsonText(value),
        idempotencyKey: options?.idempotencyKey ?? newUUIDv7(),
      }
      const response = await this.request(
        turn === undefined
          ? {
              case: "sessionOutputWriteRequested",
              value: create(programProto.SessionOutputWriteRequestedSchema, {
                ...common,
                execution: this.execution,
              }),
            }
          : {
              case: "turnOutputWriteRequested",
              value: create(programProto.TurnOutputWriteRequestedSchema, {
                ...common,
                execution: turn.scope,
                ...(callback === undefined
                  ? {}
                  : { messageDeliveryId: callback.deliveryId }),
              }),
            },
        correlationId,
        "Output write",
      )
      const receipt = parseRuntimeProtocolValue("Output receipt", () =>
        parseOutputReceipt(JSON.parse(response.dataJson)),
      )
      if (
        receipt.sessionId !== this.execution.sessionId ||
        receipt.turnId !== (turn?.scope.turnId ?? null) ||
        receipt.runId !== this.execution.runId ||
        receipt.attemptNumber !== this.execution.attemptNumber ||
        receipt.runGeneration !== Number(this.execution.runGeneration)
      ) {
        throw new RuntimeProtocolError(
          "Output receipt does not match its producer",
        )
      }
      return receipt
    }
    return Object.freeze({
      write: (value: JsonValue, options?: SessionOperationOptions) =>
        track((callback) => write(value, options, callback)),
      pipe: (source: AsyncIterable<JsonValue> | Iterable<JsonValue>) =>
        track(async (callback) => {
          for await (const value of source)
            await write(value, undefined, callback)
        }),
    })
  }

  settle(
    turn: TurnRuntime,
    disposition: "completed" | "failed",
    value?: JsonValue,
    present = true,
  ): Promise<void> {
    if (messageCallback.getStore() !== undefined)
      return Promise.reject(new Error("Message callbacks cannot settle a Turn"))
    try {
      this.assertTurn(turn)
    } catch (error) {
      return Promise.reject(error)
    }
    turn.phase = "settling"
    const settling = (async () => {
      await turn.ready
      await turn.claim
      await drainPromises(this.#mainWrites)
      this.assertOutputSafe()
      const beginId = newUUIDv7()
      await this.request(
        {
          case: "turnSettlementBeginRequested",
          value: create(programProto.TurnSettlementBeginRequestedSchema, {
            correlationId: beginId,
            execution: turn.scope,
          }),
        },
        beginId,
        "Turn settlement barrier",
      )
      await turn.callback
      this.assertOutputSafe()
      if (this.operations.controller.signal.aborted)
        throw this.operations.controller.signal.reason
      this.operations.assertDrained()
      const correlationId = newUUIDv7()
      // A stop may observe the committed DB state before its reply reaches this
      // heap. Retain the Turn until that exact settlement is acknowledged.
      this.#pendingSettlementTurn = turn
      try {
        const response = await requestRuntimeDecision(
          this.io,
          this.decisions,
          correlationId,
          {
            case: "turnSettleRequested",
            value: create(programProto.TurnSettleRequestedSchema, {
              correlationId,
              execution: turn.scope,
              targetInputSequence: turn.sequence,
              disposition,
              ...(disposition === "completed"
                ? present
                  ? { resultJson: jsonText(value) }
                  : {}
                : { errorJson: jsonText(value) }),
            }),
          },
        )
        if (
          this.stop !== undefined &&
          this.stop.turnId === undefined &&
          response.kind !== "committed"
        )
          throw new RuntimeProtocolError(
            "Null-Turn stop was not confirmed by settlement",
          )
        if (
          response.correlationId !== correlationId ||
          response.kind !== "committed"
        )
          throw new RuntimeProtocolError("Turn settlement was not acknowledged")
        if (this.stop?.turnId !== undefined)
          throw new RuntimeProtocolError(
            "Turn settlement committed after a Turn stop",
          )
        turn.phase = "settled"
        this.active = undefined
      } finally {
        this.#pendingSettlementTurn = undefined
      }
    })()
    this.#settlement = settling
    void settling.catch((error) => {
      if (!this.stoppedRejection(error)) this.#uncertain ??= error
    })
    return settling
  }

  assertOutputSafe(): void {
    if (this.#uncertain !== undefined) throw this.#uncertain
    if (this.#outputError !== undefined) {
      const code = (this.#outputError as { code?: string }).code
      if (
        this.stop === undefined ||
        (code !== "turn_stopping" && code !== "session_held")
      )
        throw this.#outputError
    }
  }

  stoppedRejection(error: unknown): boolean {
    if (this.stop === undefined) return false
    if (error === this.operations.controller.signal.reason) return true
    const code = (error as { code?: string } | null)?.code
    return (
      code === "turn_stopping" ||
      code === "session_held" ||
      code === "session_stopped"
    )
  }

  async drain(): Promise<void> {
    try {
      await this.#settlement
    } catch (error) {
      if (!this.stoppedRejection(error)) throw error
    }
    if (this.active !== undefined) this.active.phase = "settling"
    try {
      await this.active?.ready
      await this.active?.claim
    } catch (error) {
      if (!this.stoppedRejection(error)) throw error
    }
    await this.active?.callback
    await drainPromises(this.#mainWrites)
    await this.operations.drainForCompletion()
    this.assertOutputSafe()
    this.operations.assertDrained()
  }
}

async function drainPromises(pending: Set<Promise<unknown>>): Promise<void> {
  while (pending.size !== 0) await Promise.allSettled([...pending])
}

function jsonText(value: unknown): string {
  return new TextDecoder().decode(canonicalizeJsonValue(value as JsonValue))
}

function failureJSON(error: unknown): JsonValue {
  if (error instanceof Error)
    return { message: boundedUtf8(error.message, MAX_TASK_ERROR_MESSAGE_BYTES) }
  try {
    return JSON.parse(jsonText(error)) as JsonValue
  } catch {
    return { message: boundedUtf8(String(error), MAX_TASK_ERROR_MESSAGE_BYTES) }
  }
}

async function runActor(
  start: programProto.ProgramStart,
  definition: InternalActorDefinition,
  io: ProgramIO,
  decisions: ResumeDecisionRouter,
): Promise<void> {
  const operations = new RunOperationState(),
    waitGate = new ConsumingWaitGate()
  const actor = new ActorRuntime(
    start,
    definition,
    io,
    decisions,
    waitGate,
    operations,
  )
  operations.admission = () => actor.assertAdmission()
  const uninstall = installRuntimeOperations(
    programRuntimeOperations(start, io, decisions, waitGate, operations, actor),
  )
  decisions.listenForControl(
    (decision) => actor.control(decision),
    (error) => actor.uncertain(error),
  )
  let failure: unknown
  let failed = false
  try {
    await definition.handler(
      Object.freeze({
        id: actor.execution.sessionId,
        ...(start.entrypoint.case === "actor" &&
        start.entrypoint.value.key !== undefined
          ? { key: start.entrypoint.value.key }
          : {}),
        receive: (options?: ActorSessionReceiveOptions) =>
          actor.receive(options),
        output: actor.writer(),
      }),
      actorContext(start, operations.controller.signal),
    )
  } catch (error) {
    failed = true
    failure = error
  }
  try {
    await actor.drain()
    if (
      actor.stop !== undefined &&
      (!failed || actor.stoppedRejection(failure))
    ) {
      await writeRunEvent(io, {
        case: "actorOutcome",
        value: create(programProto.ActorOutcomeSchema, {
          runGeneration: actor.execution.runGeneration,
          outcome: {
            case: "interrupted",
            value: create(programProto.ActorInterruptedSchema, actor.stop),
          },
        }),
      })
    } else if (failed || actor.active !== undefined) {
      if (failure instanceof RuntimeProtocolError) throw failure
      await writeActorFailure(
        io,
        actor.execution.runGeneration,
        errorMessage(
          failure ?? new Error("Run returned with an unsettled Turn"),
        ),
      )
    } else {
      operations.assertCanComplete()
      await writeRunEvent(io, {
        case: "actorOutcome",
        value: create(programProto.ActorOutcomeSchema, {
          runGeneration: actor.execution.runGeneration,
          outcome: {
            case: "succeeded",
            value: create(programProto.ActorSucceededSchema),
          },
        }),
      })
    }
  } finally {
    decisions.stopControl()
    uninstall()
  }
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

function actorContext(
  start: programProto.ProgramStart,
  signal: AbortSignal,
): ActorContext {
  if (start.entrypoint.case !== "actor") {
    throw new Error("Actor Program-start entrypoint is required")
  }
  return Object.freeze({
    ...executionContext(start, signal),
    actor: Object.freeze({
      id: start.entrypointDeclaredId,
    }),
  }) as ActorContext
}

async function writeActorFailure(
  io: ProgramIO,
  runGeneration: bigint,
  message: string,
): Promise<void> {
  const normalizedMessage = canonicalFailureMessage(message, "actor failed")
  await writeRunEvent(io, {
    case: "actorOutcome",
    value: create(programProto.ActorOutcomeSchema, {
      runGeneration,
      outcome: {
        case: "failed",
        value: create(programProto.ActorFailedSchema, {
          message: normalizedMessage,
        }),
      },
    }),
  })
}

function taskContext(
  start: programProto.ProgramStart,
  signal = new AbortController().signal,
): TaskContext {
  return Object.freeze({
    ...executionContext(start, signal),
    task: Object.freeze({ id: start.entrypointDeclaredId }),
  }) as TaskContext
}

function executionContext(
  start: programProto.ProgramStart,
  signal: AbortSignal,
): ExecutionContext {
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
    workspace: createWorkspaceRef(start.workspaceId),
  }) as ExecutionContext
}

function runCause(cause: programProto.RunCause): RunCause {
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
        ).toISOString(),
        ...(cause.kind.value.previousScheduledAtUnixMs === undefined
          ? {}
          : {
              lastScheduledAt: new Date(
                Number(cause.kind.value.previousScheduledAtUnixMs),
              ).toISOString(),
            }),
        timezone: cause.kind.value.timezone,
      }
    case "actorStart":
      return { type: "actor_start" }
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
  const normalizedMessage = canonicalFailureMessage(message, "task failed")
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
    value: create(programProto.TaskOutcomeSchema, {
      outcome: kind === "failed"
        ? {
            case: "failed",
            value: create(programProto.TaskFailedSchema, {
              message: normalizedMessage,
              ...(detailsJson === undefined ? {} : { detailsJson }),
            }),
          }
        : {
            case: "payloadInvalid",
            value: create(programProto.TaskPayloadInvalidSchema, {
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
  if (Buffer.byteLength(value) <= maxBytes) return value
  const suffix = "…"
  const suffixBytes = Buffer.byteLength(suffix)
  let result = ""
  let size = 0
  for (const character of value) {
    const characterBytes = Buffer.byteLength(character)
    if (size + characterBytes + suffixBytes > maxBytes) break
    result += character
    size += characterBytes
  }
  return result + suffix
}

function canonicalFailureMessage(message: string, fallback: string): string {
  const canonical = trimGoSpace(message)
  return boundedUtf8(
    canonical === "" ? fallback : canonical,
    MAX_TASK_ERROR_MESSAGE_BYTES,
  )
}

async function writeRunEvent(
  io: ProgramIO,
  event: programProto.RunEvent["event"],
): Promise<void> {
  const body = toBinary(
    programProto.RunEventSchema,
    create(programProto.RunEventSchema, { event }),
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
