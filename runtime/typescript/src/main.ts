import { create, fromBinary, toBinary, toJson } from "@bufbuild/protobuf"
import { BundleSchema, runProto } from "@helmr/proto"
import {
  ConcurrentWaitError,
  WaitCancelledError,
  WaitTimeoutError,
  type TaskContext,
  type TaskWorkspace,
  type InputStreamWaitOptions,
  type InputStreamPeekOptions,
  type StreamAppendOptions,
  type StreamListOptions,
  type StreamReadOptions,
  type StreamRecord,
  type SessionContext,
  type WaitResult,
  type PayloadSchema,
  type RuntimeTokenCreateOptions,
  type DurationInput,
  type UntilInput,
  type WaitJson,
} from "@helmr/sdk"
import {
  enterRunRuntime,
  validateStreamName,
  WaitResultImpl,
  parsePayloadWithSchema,
  parseTaskPayload,
  type RuntimeTokenWaitOptions,
  type Token,
} from "@helmr/sdk/internal"
import { createWriteStream, type WriteStream } from "node:fs"
import { createConnection, type Socket } from "node:net"
import { resolve } from "node:path"
import { inspect } from "node:util"

import {
  DuplicateTaskIdError,
  MissingConfigError,
  loadDeploymentRegistry,
  loadConfig,
  loadTaskRegistry,
  type DeploymentRegistry,
} from "./config"
import {
  TaskNotFoundError,
  lookupRegisteredTask,
} from "./registry"

interface ParsedArgs {
  readonly command: string
  readonly options: Record<string, string>
}

interface AdapterWritable {
  write(chunk: string | Uint8Array): unknown
}

export interface AdapterIo {
  readonly stdin: NodeJS.ReadableStream
  readonly stdout: AdapterWritable
  readonly stderr: AdapterWritable
  readonly control?: AdapterWritable
}

type AdapterParseErrorKind =
  | "bad_request"
  | "task_not_found"
  | "duplicate_task_id"
  | "missing_config"

interface SerializedError {
  readonly level: "error"
  readonly kind: AdapterParseErrorKind
  readonly message: string
  readonly stack?: string | null
}

const processIo: AdapterIo = {
  stdin: process.stdin,
  stdout: process.stdout,
  stderr: process.stderr,
}

const RUNTIME_CONTENT_JSON_MAX_BYTES = 256 * 1024
const CHANNEL_NAME_MAX_BYTES = 256
const ADAPTER_MAX_FRAME_BYTES = 256 * 1024 * 1024
const LOG_ENTRY_MAX_BYTES = 64 * 1024
const WAIT_METADATA_JSON_MAX_BYTES = 64 * 1024
const WAIT_TAGS_MAX_COUNT = 32
const WAIT_TAG_MAX_BYTES = 128
const TRUNCATED_LOG_ENTRY_MARKER = "\n...[truncated logger entry]"

export async function runAdapterCli(
  argv: readonly string[] = process.argv.slice(2),
  io: AdapterIo = processIo,
): Promise<number> {
  try {
    const args = parseArgs(argv)
    switch (args.command) {
      case "parse":
        await parseCommand(args, io)
        break
      case "run":
        await runCommand(args, io)
        break
      case "inspect-config":
        await inspectConfigCommand(args, io)
        break
      default:
        throw new Error(`unknown adapter command: ${args.command}`)
    }
    return 0
  } catch (error: unknown) {
    writeSerializedError(io.stderr, serializeError(error))
    return 1
  }
}

async function parseCommand(args: ParsedArgs, io: AdapterIo): Promise<void> {
  const cwd = resolve(requireArg(args, "cwd"))
  const output = args.options["output"] ?? "json"
  switch (output) {
    case "json": {
      const registry = await loadDeploymentRegistry(cwd)
      io.stdout.write(`${JSON.stringify(serializeDeploymentRegistry(registry))}\n`)
      break
    }
    case "binary": {
      const registry = await loadTaskRegistry(cwd)
      const taskId = requireArg(args, "task")
      const bytes = toBinary(BundleSchema, lookupRegisteredTask(registry, taskId).bundle)
      io.stdout.write(bytes)
      break
    }
    default:
      throw new Error(`unsupported --output value: ${output}`)
  }
}

async function inspectConfigCommand(args: ParsedArgs, io: AdapterIo): Promise<void> {
  const cwd = resolve(requireArg(args, "cwd"))
  const config = await loadConfig(cwd)
  io.stdout.write(`${JSON.stringify({
    project: config.project,
    dirs: config.dirs,
    ignorePatterns: config.ignorePatterns ?? null,
  })}\n`)
}

function classifyAdapterParseErrorKind(error: Error): AdapterParseErrorKind {
  if (error instanceof MissingConfigError) {
    return "missing_config"
  }
  if (error instanceof TaskNotFoundError) {
    return "task_not_found"
  }
  if (error instanceof DuplicateTaskIdError) {
    return "duplicate_task_id"
  }
  return "bad_request"
}

function serializeError(error: unknown): SerializedError {
  if (error instanceof Error) {
    return {
      level: "error",
      kind: classifyAdapterParseErrorKind(error),
      message: error.message,
      stack: error.stack ?? null,
    }
  }
  return { level: "error", kind: "bad_request", message: String(error) }
}

function writeSerializedError(sink: AdapterWritable, error: SerializedError): void {
  sink.write(`${JSON.stringify(error)}\n`)
}

async function runCommand(args: ParsedArgs, io: AdapterIo): Promise<void> {
  const cwd = resolve(requireArg(args, "cwd"))
  process.chdir(cwd)
  const taskCwd = resolve(args.options["task-cwd"] ?? cwd)
  const taskId = requireArg(args, "task")
  const runId = requireArg(args, "run-id")
  const control = await AdapterControlWriter.open(io.control)
  const responses = new AdapterResponseReader(io.stdin)
  let leaveRuntime: (() => void) | undefined

  try {
    const registry = await loadTaskRegistry(taskCwd)
    const registeredTask = lookupRegisteredTask(registry, taskId)
    const task = registeredTask.task
    const controller = new AbortController()
    const rawPayload = parsePayload(args.options["payload-json"])
    const taskContext = parseTaskContext(requireArg(args, "task-context-json"), runId, taskId)
    const mintCorrelationId = createCorrelationIdMint()
    const waitGate = new WaitGate()
    const inputStreamWaitPositions = new Map<string, number>()
    leaveRuntime = enterRunRuntime({
      createToken: (opts: RuntimeTokenCreateOptions) =>
        createToken(responses, control, waitGate, opts),
      waitToken: <TPayload>(opts: RuntimeTokenWaitOptions) =>
        waitToken<TPayload>(responses, control, mintCorrelationId, waitGate, opts),
      inputStreamWait: <TPayload>(stream: string, schema: PayloadSchema<any, TPayload> | undefined, opts?: InputStreamWaitOptions) =>
        waitInputStream<TPayload>(responses, control, mintCorrelationId, waitGate, inputStreamWaitPositions, stream, schema, opts),
      inputStreamOnce: <TPayload>(stream: string, schema: PayloadSchema<any, TPayload> | undefined, opts?: InputStreamWaitOptions) =>
        onceInputStream<TPayload>(responses, control, mintCorrelationId, waitGate, stream, schema, opts),
      inputStreamPeek: <TPayload>(stream: string, schema: never, opts?: InputStreamPeekOptions) =>
        peekInputStream<TPayload>(responses, control, mintCorrelationId, waitGate, stream, schema, opts),
      outputStreamAppend: (stream: string, payload: unknown, opts?: StreamAppendOptions) => writeStreamOutput(control, stream, payload, opts),
      outputStreamRead: <TPayload>(stream: string, schema: never, opts?: StreamReadOptions) =>
        readOutputStream<TPayload>(stream, schema, opts),
      outputStreamList: <TPayload>(stream: string, schema: never, opts?: StreamListOptions) =>
        listOutputStream<TPayload>(stream, schema, opts),
      waitFor: (input: DurationInput) => waitFor(responses, control, mintCorrelationId, waitGate, input),
      waitUntil: (input: UntilInput) => waitUntil(responses, control, mintCorrelationId, waitGate, input),
      metadataSet: (key: string, value: unknown) => writeMetadataSet(control, key, value),
      metadataPatch: (value: Record<string, unknown>) => writeMetadataPatch(control, value),
      metadataIncrement: (key: string, amount = 1) => writeMetadataIncrement(control, key, amount),
      log: (level, values) => writeLog(control, level, values),
    })
    const ctx = {
      signal: controller.signal,
      run: taskContext.run,
      task: taskContext.task,
      workspace: taskContext.workspace,
      session: createSessionContext(taskContext.session.id),
    }
    let result: unknown
    const payload = task.payload === undefined ? undefined : await parseTaskPayload(task, rawPayload)
    try {
      if (task.payload === undefined) {
        result = await (task.run as (ctx: TaskContext) => unknown)(ctx)
      } else {
        result = await task.run(payload, ctx)
      }
    } catch (error: unknown) {
      const serialized = serializeError(error)
      writeSerializedError(io.stderr, serialized)
      await drainProcessOutputStreams()
      writeTaskResult(control, { exitCode: 1, errorMessage: serialized.message })
      return
    } finally {
      leaveRuntime()
    }
    const outputJson = stringifyTaskOutput(result)
    await drainProcessOutputStreams()
    writeTaskResult(control, outputJson === undefined ? { exitCode: 0 } : { exitCode: 0, outputJson })
  } catch (error: unknown) {
    const serialized = serializeError(error)
    writeSerializedError(io.stderr, serialized)
    await drainProcessOutputStreams()
    writeTaskResult(control, { exitCode: 1, errorMessage: serialized.message })
  } finally {
    leaveRuntime?.()
    responses.close()
    await control.close()
  }
}

function createCorrelationIdMint(): () => string {
  let nextCorrelationId = 0
  return () => {
    nextCorrelationId += 1
    return String(nextCorrelationId)
  }
}

function parsePayload(value: string | undefined): unknown {
  if (value === undefined || value === "") {
    return {}
  }
  return JSON.parse(value)
}

interface ParsedTaskContext {
  readonly run: {
    readonly id: string
    readonly attemptId?: string
    readonly attemptNumber?: number
    readonly runLeaseId?: string
    readonly snapshotVersion?: number
  }
  readonly task: { readonly id: string }
  readonly workspace: TaskWorkspace
  readonly session: {
    readonly id: string
  }
}

function parseTaskContext(json: string, runId: string, taskId: string): ParsedTaskContext {
  const parsed = JSON.parse(json)
  if (parsed === null || typeof parsed !== "object") {
    throw new Error("task context json must be an object")
  }
  const record = parsed as Record<string, unknown>
  const contextRunId = readStringField(record, "run", "id", "task context run.id")
  const contextTaskId = readStringField(record, "task", "id", "task context task.id")
  if (contextRunId !== runId) {
    throw new Error(`task context run.id ${JSON.stringify(contextRunId)} does not match --run-id ${JSON.stringify(runId)}`)
  }
  if (contextTaskId !== taskId) {
    throw new Error(`task context task.id ${JSON.stringify(contextTaskId)} does not match --task ${JSON.stringify(taskId)}`)
  }
  const workspace = parseTaskWorkspace(record["workspace"])
  const session = parseSession(record["session"])
  const runRecord = record["run"] as Record<string, unknown>
  const run = {
    id: contextRunId,
    ...optionalProperty("attemptId", readOptionalStringField(runRecord, "attemptId", "task context run.attemptId")),
    ...optionalProperty("attemptNumber", readOptionalPositiveIntegerField(runRecord, "attemptNumber", "task context run.attemptNumber")),
    ...optionalProperty("runLeaseId", readOptionalStringField(runRecord, "runLeaseId", "task context run.runLeaseId")),
    ...optionalProperty("snapshotVersion", readOptionalPositiveIntegerField(runRecord, "snapshotVersion", "task context run.snapshotVersion")),
  }
  return {
    run: Object.freeze(run),
    task: Object.freeze({ id: contextTaskId }),
    workspace: Object.freeze(workspace),
    session: Object.freeze(session),
  }
}

function readStringField(
  value: Record<string, unknown>,
  objectKey: string,
  fieldKey: string,
  label: string,
): string {
  const objectValue = value[objectKey]
  if (objectValue === null || typeof objectValue !== "object") {
    throw new Error(`${label} is required`)
  }
  const fieldValue = (objectValue as Record<string, unknown>)[fieldKey]
  if (typeof fieldValue !== "string" || fieldValue.trim() === "") {
    throw new Error(`${label} is required`)
  }
  return fieldValue
}

function readOptionalStringField(value: Record<string, unknown>, fieldKey: string, label: string): string | undefined {
  const fieldValue = value[fieldKey]
  if (fieldValue === undefined) {
    return undefined
  }
  if (typeof fieldValue !== "string" || fieldValue.trim() === "") {
    throw new Error(`${label} must be a non-empty string`)
  }
  return fieldValue
}

function readOptionalPositiveIntegerField(value: Record<string, unknown>, fieldKey: string, label: string): number | undefined {
  const fieldValue = value[fieldKey]
  if (fieldValue === undefined) {
    return undefined
  }
  if (typeof fieldValue !== "number" || !Number.isInteger(fieldValue) || fieldValue <= 0) {
    throw new Error(`${label} must be a positive integer`)
  }
  return fieldValue
}

function optionalProperty<TKey extends string, TValue>(key: TKey, value: TValue | undefined): Record<TKey, TValue> | {} {
  return value === undefined ? {} : { [key]: value } as Record<TKey, TValue>
}

function parseTaskWorkspace(value: unknown): TaskWorkspace {
  if (value === null || typeof value !== "object") {
    throw new Error("task context workspace is required")
  }
  const record = value as Record<string, unknown>
  return {
    path: readRequiredString(record, "path", "task context workspace.path"),
    projectPath: readRequiredString(record, "projectPath", "task context workspace.projectPath"),
  }
}

function parseSession(value: unknown): { readonly id: string } {
  if (value === null || typeof value !== "object") {
    throw new Error("task context session is required")
  }
  const record = value as Record<string, unknown>
  return {
    id: readRequiredString(record, "id", "task context session.id"),
  }
}

function readRequiredString(record: Record<string, unknown>, key: string, label: string): string {
  const value = record[key]
  if (typeof value !== "string" || value.trim() === "") {
    throw new Error(`${label} is required`)
  }
  return value
}

function serializeDeploymentRegistry(registry: DeploymentRegistry): {
  readonly tasks: Record<string, {
    readonly originFile: string
    readonly modulePath: string
    readonly exportName: string
    readonly bundle: unknown
  }>
  readonly streams: readonly {
    readonly name: string
    readonly direction: "input" | "output"
    readonly schema_fingerprint?: string
    readonly schema_json: unknown
  }[]
  readonly queues: readonly {
    readonly name: string
    readonly concurrency_limit?: number
  }[]
} {
  return {
    tasks: Object.fromEntries(
      [...registry.tasks.entries()]
        .sort(([leftId], [rightId]) => compareAscii(leftId, rightId))
        .map(([taskId, task]) => [
          taskId,
          {
            originFile: task.originFile,
            modulePath: task.modulePath,
            exportName: task.exportName,
            bundle: toJson(BundleSchema, task.bundle),
          },
        ]),
    ),
    streams: registry.streams.map((stream) => ({
      name: stream.name,
      direction: stream.direction,
      ...(stream.schemaFingerprint === "" ? {} : { schema_fingerprint: stream.schemaFingerprint }),
      schema_json: JSON.parse(stream.schemaJson),
    })),
    queues: registry.queues.map((queue) => ({
      name: queue.name,
      ...(queue.concurrencyLimit === undefined ? {} : { concurrency_limit: queue.concurrencyLimit }),
    })),
  }
}

function compareAscii(left: string, right: string): number {
  if (left < right) return -1
  if (left > right) return 1
  return 0
}

interface RuntimeWaitRequest {
  readonly kind: string
  readonly paramsJson: string
  readonly metadataJson?: string
  readonly tags?: string[]
  readonly timeout?: number
  readonly idleTimeout?: number
}

interface RuntimeWaitOptions {
  readonly timeout?: number
  readonly idleTimeout?: number
  readonly metadata?: { readonly [key: string]: WaitJson }
  readonly tags?: string | readonly string[]
}

class AdapterResponseReader {
  readonly #iterator: AsyncIterator<Uint8Array>
  #buffer = Buffer.alloc(0)
  #closed = false

  constructor(stdin: NodeJS.ReadableStream) {
    this.#iterator = (stdin as AsyncIterable<Uint8Array>)[Symbol.asyncIterator]()
  }

  close(): void {
    this.#closed = true
  }

  async readDecision(): Promise<runProto.ResumeDecision> {
    if (this.#closed) {
      throw new Error("adapter response stream closed")
    }
    const body = await this.#readFrameBody()
    return fromBinary(runProto.ResumeDecisionSchema, body)
  }

  async readTokenCreateResult(): Promise<runProto.TokenCreateResult> {
    if (this.#closed) {
      throw new Error("adapter response stream closed")
    }
    const body = await this.#readFrameBody()
    return fromBinary(runProto.TokenCreateResultSchema, body)
  }

  async readActiveStreamReadResult(): Promise<runProto.ActiveStreamReadResult> {
    if (this.#closed) {
      throw new Error("adapter response stream closed")
    }
    const body = await this.#readFrameBody()
    return fromBinary(runProto.ActiveStreamReadResultSchema, body)
  }

  async #readFrameBody(): Promise<Uint8Array> {
    await this.#fill(4)
    const len = this.#buffer.readUInt32BE(0)
    this.#buffer = this.#buffer.subarray(4)
    if (len > ADAPTER_MAX_FRAME_BYTES) {
      throw new Error(`adapter response frame length ${len} exceeds max ${ADAPTER_MAX_FRAME_BYTES}`)
    }
    await this.#fill(len)
    const body = this.#buffer.subarray(0, len)
    this.#buffer = this.#buffer.subarray(len)
    return body
  }

  async #fill(bytes: number): Promise<void> {
    while (this.#buffer.length < bytes) {
      const next = await this.#iterator.next()
      if (next.done === true) {
        this.#closed = true
        throw new Error("adapter response stream closed")
      }
      this.#buffer = Buffer.concat([this.#buffer, Buffer.from(next.value)])
    }
  }
}

class WaitGate {
  #inFlight = false

  async run<T>(fn: () => Promise<T>): Promise<T> {
    if (this.#inFlight) {
      throw new ConcurrentWaitError("concurrent blocking run I/O calls are not supported")
    }
    this.#inFlight = true
    try {
      return await fn()
    } finally {
      this.#inFlight = false
    }
  }
}

function createSessionContext(
  id: string,
): SessionContext {
  return Object.freeze({
    id,
  })
}

class AdapterControlWriter {
  static async open(sink?: AdapterWritable): Promise<AdapterControlWriter> {
    if (sink !== undefined) {
      return new AdapterControlWriter({ sink })
    }
    const fd = process.env["HELMR_CONTROL_FD"]?.trim()
    delete process.env["HELMR_CONTROL_FD"]
    if (fd) {
      const controlFd = Number.parseInt(fd, 10)
      if (!Number.isSafeInteger(controlFd) || controlFd < 3) {
        throw new Error(`invalid HELMR_CONTROL_FD: ${fd}`)
      }
      return new AdapterControlWriter({ stream: createWriteStream("/dev/null", { fd: controlFd }) })
    }
    const socketPath = process.env["HELMR_CONTROL_SOCKET"]?.trim()
    delete process.env["HELMR_CONTROL_SOCKET"]
    if (!socketPath) {
      throw new Error("HELMR_CONTROL_SOCKET is required")
    }
    return new AdapterControlWriter({ socket: await connectControlSocket(socketPath) })
  }

  readonly #target: { readonly socket: Socket } | { readonly stream: WriteStream } | { readonly sink: AdapterWritable }

  private constructor(target: { readonly socket: Socket } | { readonly stream: WriteStream } | { readonly sink: AdapterWritable }) {
    this.#target = target
  }

  write(event: runProto.RunEvent): void {
    const body = Buffer.from(toBinary(runProto.RunEventSchema, event))
    const header = Buffer.alloc(4)
    header.writeUInt32BE(body.length, 0)
    const frame = Buffer.concat([header, body])
    if ("socket" in this.#target) {
      this.#target.socket.write(frame)
    } else if ("stream" in this.#target) {
      this.#target.stream.write(frame)
    } else {
      this.#target.sink.write(frame)
    }
  }

  close(): Promise<void> {
    const target = this.#target
    if ("socket" in target) {
      return new Promise((resolveClose) => {
        target.socket.end(resolveClose)
      })
    }
    if ("stream" in target) {
      return new Promise((resolveClose) => {
        target.stream.end(resolveClose)
      })
    }
    return Promise.resolve()
  }
}

function connectControlSocket(socketPath: string): Promise<Socket> {
  return new Promise((resolveConnection, rejectConnection) => {
    const socket = createConnection(socketPath)
    const onError = (error: Error) => {
      socket.destroy()
      rejectConnection(error)
    }
    socket.once("error", onError)
    socket.once("connect", () => {
      socket.off("error", onError)
      resolveConnection(socket)
    })
  })
}

async function createToken(
	responses: AdapterResponseReader,
	control: AdapterControlWriter,
	waitGate: WaitGate,
	opts: RuntimeTokenCreateOptions,
): Promise<Token> {
	return waitGate.run(async () => createTokenUnlocked(responses, control, opts))
}

async function createTokenUnlocked(
	responses: AdapterResponseReader,
	control: AdapterControlWriter,
	opts: RuntimeTokenCreateOptions,
): Promise<Token> {
  const metadata = opts.metadata === undefined ? undefined : normalizeWaitMetadata(opts.metadata)
  const metadataJson = metadata === undefined ? undefined : JSON.stringify(metadata)
  const tags = opts.tags === undefined ? undefined : normalizeWaitTags(opts.tags)
  const timeoutInSeconds = opts.timeout === undefined ? undefined : waitDurationSeconds(opts.timeout)
  control.write(create(runProto.RunEventSchema, {
    event: {
      case: "tokenCreateRequested",
      value: create(runProto.TokenCreateRequestedSchema, {
        ...(timeoutInSeconds === undefined ? {} : { timeoutInSeconds }),
        ...(tags === undefined ? {} : { tags }),
        ...(metadataJson === undefined ? {} : { metadataJson }),
      }),
    },
  }))
  const result = await responses.readTokenCreateResult()
  if (result.errorMessage !== undefined && result.errorMessage.trim() !== "") {
    throw new Error(result.errorMessage)
  }
  if (result.id.trim() === "") {
    throw new Error("token create response id is required")
  }
  if (result.callbackUrl.trim() === "") {
    throw new Error("token create response callback_url is required")
  }
  const resultMetadata = result.metadataJson === undefined || result.metadataJson.trim() === ""
    ? undefined
    : parseTokenMetadata(result.metadataJson)
	const status = tokenStatus(result.status)
	return {
		id: result.id,
		callbackUrl: result.callbackUrl,
		...(result.publicAccessToken === undefined ? {} : { publicAccessToken: result.publicAccessToken }),
		timeoutAt: result.timeoutAt ?? null,
		...(status === undefined ? {} : { status }),
		...(result.tags.length === 0 ? {} : { tags: result.tags }),
		...(resultMetadata === undefined ? {} : { metadata: resultMetadata }),
		wait: () => {
      throw new Error("token.wait() is attached by @helmr/sdk")
    },
  }
}

function tokenStatus(value: string | undefined): Token["status"] | undefined {
  switch (value) {
    case "waiting":
      return "pending"
    case "pending":
    case "completed":
    case "expired":
    case "cancelled":
      return value
    case "timed_out":
      return "expired"
    case undefined:
    case "":
      return undefined
    default:
      throw new Error(`token create response status is invalid: ${value}`)
  }
}

async function waitFor(
  responses: AdapterResponseReader,
  control: AdapterControlWriter,
  mintCorrelationId: () => string,
  waitGate: WaitGate,
  input: DurationInput,
): Promise<void> {
  const seconds = waitDurationSeconds(input)
  const decision = await waitGenericDecision(responses, control, mintCorrelationId, waitGate, waitRequest(
    "timer",
    normalizeDurationInput(input),
    { timeout: seconds },
  ))
  if (!(decision.kind === "timed_out" || decision.kind === "completed")) {
    throw new Error(`unexpected wait.for resume decision kind ${JSON.stringify(decision.kind)}`)
  }
  maybeWriteResumeConsumed(control, decision)
}

async function waitUntil(
  responses: AdapterResponseReader,
  control: AdapterControlWriter,
  mintCorrelationId: () => string,
  waitGate: WaitGate,
  input: UntilInput,
): Promise<void> {
  const until = waitUntilInputDate(input)
  const seconds = Math.ceil((until.getTime() - Date.now()) / 1000)
  if (seconds <= 0) {
    return
  }
  const decision = await waitGenericDecision(responses, control, mintCorrelationId, waitGate, waitRequest(
    "timer",
    normalizeUntilInput(input),
    { timeout: seconds },
  ))
  if (!(decision.kind === "timed_out" || decision.kind === "completed")) {
    throw new Error(`unexpected wait.until resume decision kind ${JSON.stringify(decision.kind)}`)
  }
  maybeWriteResumeConsumed(control, decision)
}

async function waitToken<TPayload>(
  responses: AdapterResponseReader,
  control: AdapterControlWriter,
  mintCorrelationId: () => string,
  waitGate: WaitGate,
  opts: RuntimeTokenWaitOptions,
): Promise<WaitResultImpl<TPayload>> {
  const decision = await waitGenericDecision(responses, control, mintCorrelationId, waitGate, waitRequest(
    "token",
    { token_id: opts.tokenId },
    waitOptionsToRequest(opts),
  ))
	if (decision.kind === "timed_out") {
		const seconds = opts.timeout === undefined ? undefined : waitDurationSeconds(opts.timeout)
		maybeWriteResumeConsumed(control, decision)
		return new WaitResultImpl<TPayload>(false, undefined as TPayload, new WaitTimeoutError(`token wait timed out${formatTimeoutSuffix(seconds)}`, seconds))
	}
	if (decision.kind === "cancelled") {
		maybeWriteResumeConsumed(control, decision)
		return new WaitResultImpl<TPayload>(false, undefined as TPayload, new WaitCancelledError("token cancelled"))
	}
	if (decision.kind !== "completed") {
		throw new Error(`unexpected token wait resume decision kind ${JSON.stringify(decision.kind)}`)
	}
  const data = parseResumeData(decision.dataJson)
  if (opts.schema === undefined) {
    maybeWriteResumeConsumed(control, decision)
    return new WaitResultImpl(true, data as TPayload)
  }
  const payload = await parsePayloadWithSchema(opts.schema, data, "token data") as TPayload
  maybeWriteResumeConsumed(control, decision)
  return new WaitResultImpl(true, payload)
}

async function waitInputStream<TPayload>(
	responses: AdapterResponseReader,
	control: AdapterControlWriter,
	mintCorrelationId: () => string,
	waitGate: WaitGate,
	positions: Map<string, number>,
	stream: string,
	schema: PayloadSchema<any, TPayload> | undefined,
	opts: {
		readonly correlationId?: string
		readonly afterSequence?: number
		readonly timeout?: DurationInput
    readonly idleTimeout?: DurationInput
    readonly metadata?: { readonly [key: string]: WaitJson }
    readonly tags?: string | readonly string[]
  } = {},
): Promise<WaitResult<TPayload>> {
  const correlationId = normalizeOptionalCorrelationId(opts.correlationId)
  const positionKey = inputStreamPositionKey(stream, correlationId)
  const afterSequence = opts.afterSequence === undefined
    ? positions.get(positionKey)
    : normalizeStreamSequence(opts.afterSequence, "input stream wait afterSequence")
  const decision = await waitGenericDecision(responses, control, mintCorrelationId, waitGate, waitRequest(
    "stream",
    {
      stream,
      ...(correlationId === undefined ? {} : { correlation_id: correlationId }),
      ...(afterSequence === undefined ? {} : { after_sequence: afterSequence }),
    },
    waitOptionsToRequest(opts),
  ))
	if (decision.kind === "timed_out") {
		const seconds = opts.timeout === undefined ? undefined : waitDurationSeconds(opts.timeout)
		maybeWriteResumeConsumed(control, decision)
		return new WaitResultImpl<TPayload>(false, undefined as TPayload, new WaitTimeoutError(`input stream ${JSON.stringify(stream)} wait timed out${formatTimeoutSuffix(seconds)}`, seconds))
	}
  if (decision.kind !== "completed") {
    throw new Error(`unexpected input stream wait resume decision kind ${JSON.stringify(decision.kind)}`)
  }
	const envelope = streamWaitEnvelope(parseResumeData(decision.dataJson), stream)
	const data = schema === undefined
		? envelope.data as TPayload
		: await parsePayloadWithSchema(schema, envelope.data, `input stream ${JSON.stringify(stream)} data`) as TPayload
  const previous = positions.get(positionKey) ?? 0
  if (envelope.sequence > previous) {
    positions.set(positionKey, envelope.sequence)
  }
	maybeWriteResumeConsumed(control, decision)
	return completedWaitResult(data)
}

function inputStreamPositionKey(stream: string, correlationId: string | undefined): string {
  return `${stream}\u0000${correlationId ?? ""}`
}

function normalizeOptionalCorrelationId(value: InputStreamWaitOptions["correlationId"]): string | undefined {
  if (value === undefined) {
    return undefined
  }
  const normalized = value.trim()
  return normalized === "" ? undefined : normalized
}

function completedWaitResult<TPayload>(data: TPayload): WaitResult<TPayload> {
	return new WaitResultImpl(true, data)
}

function streamWaitEnvelope(value: unknown, expectedStream: string): { readonly stream: string; readonly sequence: number; readonly data: unknown } {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("stream wait data must be an object")
  }
  const record = value as { readonly stream?: unknown; readonly sequence?: unknown; readonly data?: unknown }
  if (record.stream !== expectedStream) {
    throw new Error(`stream wait stream mismatch: expected ${JSON.stringify(expectedStream)}`)
  }
  if (typeof record.sequence !== "number" || !Number.isInteger(record.sequence) || record.sequence < 0) {
    throw new Error("stream wait sequence must be a non-negative integer")
  }
  return {
    stream: record.stream,
    sequence: record.sequence,
    data: record.data,
  }
}

async function waitGenericDecision(
  responses: AdapterResponseReader,
  control: AdapterControlWriter,
  mintCorrelationId: () => string,
  waitGate: WaitGate,
  request: RuntimeWaitRequest,
): Promise<runProto.ResumeDecision> {
  return waitGate.run(async () => {
    const correlationId = mintCorrelationId()
    control.write(runWaitRequestedEvent({ ...request, correlationId }))
    return responses.readDecision()
  })
}

function waitOptionsToRequest(opts: {
  readonly timeout?: DurationInput
  readonly idleTimeout?: DurationInput
  readonly metadata?: { readonly [key: string]: WaitJson }
  readonly tags?: string | readonly string[]
} | undefined): RuntimeWaitOptions {
  return {
    ...(opts?.timeout === undefined ? {} : { timeout: waitDurationSeconds(opts.timeout) }),
    ...(opts?.idleTimeout === undefined ? {} : { idleTimeout: waitDurationSeconds(opts.idleTimeout) }),
    ...(opts?.metadata === undefined ? {} : { metadata: opts.metadata }),
    ...(opts?.tags === undefined ? {} : { tags: opts.tags }),
  }
}

function waitRequest(kind: string, data: WaitJson, opts?: RuntimeWaitOptions): RuntimeWaitRequest {
  const normalizedKind = normalizeWaitKind(kind)
  const timeout = opts?.timeout
  if (timeout !== undefined) {
    validateWaitTimeout(timeout)
  }
  const idleTimeout = opts?.idleTimeout
  if (idleTimeout !== undefined) {
    validateWaitTimeout(idleTimeout)
  }
  const tags = normalizeWaitTags(opts?.tags)
  return {
    kind: normalizedKind,
    paramsJson: JSON.stringify(data),
    ...(opts?.metadata === undefined ? {} : { metadataJson: JSON.stringify(normalizeWaitMetadata(opts.metadata)) }),
    ...(tags === undefined ? {} : { tags }),
    ...(timeout === undefined ? {} : { timeout }),
    ...(idleTimeout === undefined ? {} : { idleTimeout }),
  }
}

function runWaitRequestedEvent(request: RuntimeWaitRequest & { readonly correlationId: string }): runProto.RunEvent {
  const value = runWaitRequestedValue(request)
  return create(runProto.RunEventSchema, {
    event: {
      case: "runWaitRequested",
      value,
    },
  })
}

function runWaitRequestedValue(request: RuntimeWaitRequest & { readonly correlationId: string }): runProto.RunWaitRequested {
  return create(runProto.RunWaitRequestedSchema, {
    correlationId: request.correlationId,
    kind: request.kind,
    paramsJson: request.paramsJson,
    ...(request.metadataJson === undefined ? {} : { metadataJson: request.metadataJson }),
    ...(request.tags === undefined ? {} : { tags: request.tags }),
    ...(request.timeout === undefined ? {} : { timeout: request.timeout }),
    ...(request.idleTimeout === undefined ? {} : { idleTimeout: request.idleTimeout }),
  })
}

function maybeWriteResumeConsumed(control: AdapterControlWriter, decision: runProto.ResumeDecision): void {
  if (!decision.requireConsumedAck) {
    return
  }
  if (decision.runWaitId.trim() === "") {
    throw new Error("resume decision run_wait_id is required")
  }
  control.write(create(runProto.RunEventSchema, {
    event: {
      case: "resumeConsumed",
      value: {
        runWaitId: decision.runWaitId,
      },
    },
  }))
}

function formatTimeoutSuffix(timeout: number | undefined): string {
  return timeout === undefined ? "" : ` after ${timeout}`
}

function normalizeOptionalIdentifier(value: string | undefined, label: string): string | undefined {
  if (value === undefined) return undefined
  const normalized = value.trim()
  if (normalized === "") {
    throw new Error(`${label} must be non-empty`)
  }
  return normalized
}

function normalizeDurationInput(input: DurationInput): WaitJson {
  if (typeof input === "string") {
    return { duration: input }
  }
  if (typeof input === "number") {
    return { seconds: input }
  }
  return normalizeWaitJson(input, "wait duration input")
}

function waitDurationSeconds(input: DurationInput): number {
  if (typeof input === "number") {
    return positiveDelaySeconds(input)
  }
  if (typeof input === "string") {
    return parseDurationSeconds(input, "wait duration")
  }
  const seconds = input.seconds
  if (seconds !== undefined) {
    return positiveDelaySeconds(seconds)
  }
  const milliseconds = input.milliseconds
  if (milliseconds !== undefined) {
    return positiveDelaySeconds(milliseconds / 1000)
  }
  const minutes = input.minutes
  if (minutes !== undefined) {
    return positiveDelaySeconds(minutes * 60)
  }
  const hours = input.hours
  if (hours !== undefined) {
    return positiveDelaySeconds(hours * 3600)
  }
  const duration = input.duration
  if (duration !== undefined) {
    return parseDurationSeconds(duration, "wait duration")
  }
  throw new Error("wait duration requires seconds, milliseconds, minutes, hours, or duration")
}

function parseDurationSeconds(value: string, label: string): number {
  const match = /^(\d+(?:\.\d+)?)(ms|s|m|h)$/.exec(value.trim())
  if (match === null) {
    throw new Error(`${label} must use ms, s, m, or h units`)
  }
  const amount = Number(match[1])
  const unit = match[2]
  const multiplier = unit === "ms" ? 0.001 : unit === "s" ? 1 : unit === "m" ? 60 : 3600
  return positiveDelaySeconds(amount * multiplier)
}

function normalizeUntilInput(input: UntilInput): WaitJson {
  if (typeof input === "string") {
    return { date: input }
  }
  if (input instanceof Date) {
    return { date: input.toISOString() }
  }
  return normalizeWaitJson(input, "wait.until input")
}

function waitUntilInputDate(input: UntilInput): Date {
  const value = typeof input === "object" && !(input instanceof Date) ? input.date : input
  if (value === undefined) {
    throw new Error("wait.until requires a date")
  }
  const date = value instanceof Date ? value : new Date(value)
  if (Number.isNaN(date.getTime())) {
    throw new Error("wait.until date must be a valid timestamp")
  }
  return date
}

function positiveDelaySeconds(value: number): number {
  if (!Number.isFinite(value) || value <= 0) {
    throw new Error(`invalid wait timeout: ${value}`)
  }
  const seconds = Math.ceil(value)
  validateWaitTimeout(seconds)
  return seconds
}

function normalizeWaitJson(value: unknown, label: string): WaitJson {
  if (value === null || typeof value === "boolean" || typeof value === "string") {
    return value
  }
  if (typeof value === "number") {
    if (!Number.isFinite(value)) {
      throw new Error(`${label} number must be finite`)
    }
    return value
  }
  if (value instanceof Date) {
    return value.toISOString()
  }
  if (Array.isArray(value)) {
    return value.map((item) => normalizeWaitJson(item, label))
  }
  if (typeof value === "object" && value !== undefined) {
    const entries: [string, WaitJson][] = []
    for (const [key, item] of Object.entries(value)) {
      if (item === undefined) {
        continue
      }
      entries.push([key, normalizeWaitJson(item, label)])
    }
    return Object.fromEntries(entries)
  }
  throw new Error(`${label} must be JSON-serializable`)
}

function normalizeWaitKind(value: string): string {
  const kind = value.trim()
  if (kind === "") {
    throw new Error("wait kind must be non-empty")
  }
  return kind
}

function normalizeWaitTags(value: string | readonly string[] | undefined): string[] | undefined {
  if (value === undefined) return undefined
  const tags = typeof value === "string" ? [value] : [...value]
  if (tags.length > WAIT_TAGS_MAX_COUNT) {
    throw new Error(`wait tags has ${tags.length} entries, exceeds max ${WAIT_TAGS_MAX_COUNT}`)
  }
  return tags.map((tag) => {
    const normalized = normalizeRequiredIdentifier(tag, "wait tag")
    validateUtf8ByteLength("wait tag", normalized, WAIT_TAG_MAX_BYTES)
    return normalized
  })
}

function parseTokenMetadata(value: string): Record<string, unknown> {
  const parsed = JSON.parse(value) as unknown
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error("token create response metadata_json must be a JSON object")
  }
  return parsed as Record<string, unknown>
}

function normalizeWaitMetadata(value: unknown): { readonly [key: string]: WaitJson } {
  const normalized = normalizeWaitJson(value as WaitJson, "wait metadata")
  if (normalized === null || typeof normalized !== "object" || Array.isArray(normalized)) {
    throw new Error("wait metadata must be a JSON object")
  }
  const metadataJson = JSON.stringify(normalized)
  validateUtf8ByteLength("wait metadata_json", metadataJson, WAIT_METADATA_JSON_MAX_BYTES)
  return normalized as { readonly [key: string]: WaitJson }
}

function normalizeRequiredIdentifier(value: string, label: string): string {
  const normalized = normalizeOptionalIdentifier(value, label)
  if (normalized === undefined) {
    throw new Error(`${label} is required`)
  }
  return normalized
}

function parseResumeData(json: string): unknown {
  if (json === "") {
    throw new Error("run wait data is required")
  }
  try {
    return JSON.parse(json)
  } catch (error) {
    if (error instanceof Error) {
      throw new Error(`run wait data must be valid JSON: ${error.message}`)
    }
    throw new Error("run wait data must be valid JSON")
  }
}

async function onceInputStream<TPayload>(
	responses: AdapterResponseReader,
	control: AdapterControlWriter,
	mintCorrelationId: () => string,
	waitGate: WaitGate,
	stream: string,
	schema: PayloadSchema<any, TPayload> | undefined,
	opts: InputStreamWaitOptions = {},
): Promise<WaitResult<TPayload>> {
	const record = await activeReadInputStream<TPayload>(responses, control, mintCorrelationId, waitGate, stream, schema, opts, true)
	if (record === null) {
		const seconds = opts.timeout === undefined ? undefined : waitDurationSeconds(opts.timeout)
		return new WaitResultImpl<TPayload>(false, undefined as TPayload, new WaitTimeoutError(`input stream ${JSON.stringify(stream)} once timed out${formatTimeoutSuffix(seconds)}`, seconds))
	}
	return completedWaitResult(record.data)
}

async function peekInputStream<TPayload>(
	responses: AdapterResponseReader,
	control: AdapterControlWriter,
	mintCorrelationId: () => string,
	waitGate: WaitGate,
	stream: string,
	schema: PayloadSchema<any, TPayload> | undefined,
	opts?: InputStreamPeekOptions,
): Promise<StreamRecord<TPayload> | null> {
	const block = (opts as { readonly block?: boolean } | undefined)?.block === true
	return activeReadInputStream<TPayload>(responses, control, mintCorrelationId, waitGate, stream, schema, opts ?? {}, block)
}

async function activeReadInputStream<TPayload>(
	responses: AdapterResponseReader,
	control: AdapterControlWriter,
	mintCorrelationId: () => string,
	waitGate: WaitGate,
	stream: string,
	schema: PayloadSchema<any, TPayload> | undefined,
	opts: InputStreamPeekOptions | InputStreamWaitOptions,
	block: boolean,
): Promise<StreamRecord<TPayload> | null> {
	const runRead = async () => {
		const correlationId = mintCorrelationId()
		const timeoutSeconds = block && "timeout" in opts && opts.timeout !== undefined
			? waitDurationSeconds(opts.timeout)
			: undefined
		const recordCorrelationId = opts.correlationId === undefined ? undefined : normalizeOptionalCorrelationId(opts.correlationId)
		control.write(create(runProto.RunEventSchema, {
			event: {
				case: "activeStreamReadRequested",
				value: create(runProto.ActiveStreamReadRequestedSchema, {
					correlationId,
					stream,
					afterSequence: BigInt(normalizeStreamSequence(opts.afterSequence ?? 0, "input stream afterSequence")),
					...(recordCorrelationId === undefined ? {} : { recordCorrelationId }),
					...(timeoutSeconds === undefined ? {} : { timeout: timeoutSeconds }),
					block,
				}),
			},
		}))
		const result = await responses.readActiveStreamReadResult()
		if (result.correlationId !== correlationId) {
			throw new Error(`active stream read result correlation ${JSON.stringify(result.correlationId)} did not match ${JSON.stringify(correlationId)}`)
		}
		if (result.errorMessage !== undefined && result.errorMessage !== "") {
			throw new Error(result.errorMessage)
		}
		if (result.timedOut || result.record === undefined) {
			return null
		}
		return protoStreamRecord<TPayload>(result.record, schema, stream)
	}
	return waitGate.run(runRead)
}

async function protoStreamRecord<TPayload>(
	record: runProto.StreamRecord,
	schema: PayloadSchema<any, TPayload> | undefined,
	stream: string,
): Promise<StreamRecord<TPayload>> {
	const data = parseResumeData(record.dataJson)
	const parsed = schema === undefined
		? data as TPayload
		: await parsePayloadWithSchema(schema, data, `input stream ${JSON.stringify(stream)} data`) as TPayload
	return {
		id: record.id,
		streamId: record.streamId,
		sequence: Number(record.sequence),
		data: parsed,
		...(record.correlationId === undefined || record.correlationId === "" ? {} : { correlationId: record.correlationId }),
		contentType: record.contentType,
		createdAt: record.createdAt,
	}
}

function normalizeStreamSequence(value: number, label: string): number {
	if (!Number.isInteger(value) || value < 0) {
		throw new Error(`${label} must be a non-negative integer`)
	}
	return value
}

async function readOutputStream<TPayload>(_stream: string, _schema: unknown, _opts?: StreamReadOptions): Promise<StreamRecord<TPayload> | null> {
  throw new Error("output stream read requires runtime stream read support")
}

async function listOutputStream<TPayload>(_stream: string, _schema: unknown, _opts?: StreamListOptions): Promise<StreamRecord<TPayload>[]> {
  throw new Error("output stream list requires runtime stream read support")
}

async function writeStreamOutput(control: AdapterControlWriter, streamInput: string, payload: unknown, opts: StreamAppendOptions = {}): Promise<void> {
  const stream = validateStreamName(streamInput)
  validateUtf8ByteLength("stream", stream, CHANNEL_NAME_MAX_BYTES)
  const contentType = opts.contentType?.trim()
  if (contentType !== undefined && contentType === "") {
    throw new Error("output stream contentType must be non-empty")
  }
  const payloadJson = JSON.stringify(payload === undefined ? null : payload)
  validateUtf8ByteLength("output stream payload_json", payloadJson, RUNTIME_CONTENT_JSON_MAX_BYTES)
  control.write(create(runProto.RunEventSchema, {
    event: {
      case: "outputStreamAppended",
      value: {
        stream,
        payloadJson,
        ...(contentType === undefined ? {} : { contentType }),
      },
    },
  }))
}

async function writeMetadataSet(control: AdapterControlWriter, key: string, value: unknown): Promise<void> {
  const normalizedKey = normalizeRequiredIdentifier(key, "metadata key")
  const valueJson = JSON.stringify(normalizeWaitJson(value, "metadata value"))
  validateUtf8ByteLength("metadata value_json", valueJson, RUNTIME_CONTENT_JSON_MAX_BYTES)
  control.write(create(runProto.RunEventSchema, {
    event: {
      case: "metadataUpdated",
      value: {
        operation: "set",
        key: normalizedKey,
        valueJson,
      },
    },
  }))
}

async function writeMetadataPatch(control: AdapterControlWriter, patch: Record<string, unknown>): Promise<void> {
  const payloadJson = JSON.stringify(normalizeWaitJson(patch, "metadata patch"))
  validateUtf8ByteLength("metadata patch_json", payloadJson, RUNTIME_CONTENT_JSON_MAX_BYTES)
  control.write(create(runProto.RunEventSchema, {
    event: {
      case: "metadataUpdated",
      value: {
        operation: "patch",
        patchJson: payloadJson,
      },
    },
  }))
}

async function writeMetadataIncrement(control: AdapterControlWriter, key: string, amount: number): Promise<void> {
  const normalizedKey = normalizeRequiredIdentifier(key, "metadata key")
  if (!Number.isFinite(amount)) {
    throw new Error("metadata increment amount must be finite")
  }
  control.write(create(runProto.RunEventSchema, {
    event: {
      case: "metadataUpdated",
      value: {
        operation: "increment",
        key: normalizedKey,
        amount,
      },
    },
  }))
}

function validateWaitTimeout(value: number): void {
  if (!Number.isInteger(value) || !Number.isFinite(value) || value < 1) {
    throw new Error(`invalid wait timeout: ${value}`)
  }
}

function validateUtf8ByteLength(field: string, value: string, maxBytes: number): void {
  const bytes = Buffer.byteLength(value, "utf8")
  if (bytes > maxBytes) {
    throw new Error(`${field} is ${bytes} bytes, exceeds max ${maxBytes}`)
  }
}

function writeLog(
  control: AdapterControlWriter,
  level: "info" | "warn" | "error",
  values: readonly unknown[],
): void {
  const entry = formatLogEntry(level, formatMessage(values))
  control.write(create(runProto.RunEventSchema, {
    event: {
      case: "logEntry",
      value: entry,
    },
  }))
}

function stringifyTaskOutput(result: unknown): string | undefined {
  if (result === undefined) return undefined
  return JSON.stringify(result)
}

function writeTaskResult(
  control: AdapterControlWriter,
  result: { readonly exitCode: number; readonly errorMessage?: string; readonly outputJson?: string },
): void {
  control.write(create(runProto.RunEventSchema, {
    event: {
      case: "taskResult",
      value: create(runProto.TaskResultSchema, {
        exitCode: result.exitCode,
        ...(result.errorMessage === undefined ? {} : { errorMessage: result.errorMessage }),
        ...(result.errorMessage === undefined
          ? {}
          : {
              error: {
                type: "Error",
                code: "task_error",
                message: result.errorMessage,
                retryable: false,
                detailsJson: "{}",
              },
            }),
        ...(result.outputJson === undefined ? {} : { outputJson: result.outputJson }),
      }),
    },
  }))
}

function formatLogEntry(level: "info" | "warn" | "error", message: string): string {
  const initial = JSON.stringify({ level, message })
  if (Buffer.byteLength(initial, "utf8") <= LOG_ENTRY_MAX_BYTES) {
    return initial
  }

  const markerOnly = JSON.stringify({ level, message: TRUNCATED_LOG_ENTRY_MARKER })
  let prefixBudget = Math.max(0, LOG_ENTRY_MAX_BYTES - Buffer.byteLength(markerOnly, "utf8"))
  let truncated = `${truncateUtf8Bytes(message, prefixBudget)}${TRUNCATED_LOG_ENTRY_MARKER}`
  let entry = JSON.stringify({ level, message: truncated })
  while (Buffer.byteLength(entry, "utf8") > LOG_ENTRY_MAX_BYTES && prefixBudget > 0) {
    prefixBudget -= 1
    truncated = `${truncateUtf8Bytes(message, prefixBudget)}${TRUNCATED_LOG_ENTRY_MARKER}`
    entry = JSON.stringify({ level, message: truncated })
  }
  return entry
}

function truncateUtf8Bytes(value: string, maxBytes: number): string {
  let used = 0
  let out = ""
  for (const char of value) {
    const bytes = Buffer.byteLength(char, "utf8")
    if (used + bytes > maxBytes) break
    used += bytes
    out += char
  }
  return out
}

function formatMessage(values: readonly unknown[]): string {
  return values
    .map((value) => (typeof value === "string" ? value : inspect(value, { breakLength: Infinity })))
    .join(" ")
}

function parseArgs(argv: readonly string[]): ParsedArgs {
  const [command, ...rest] = argv
  if (!command) {
    throw new Error("missing command")
  }

  const options: Record<string, string> = {}
  for (let index = 0; index < rest.length; index += 2) {
    const key = rest[index]
    const value = rest[index + 1]
    if (!key?.startsWith("--") || value === undefined) {
      throw new Error(`invalid arguments near ${key ?? "<eof>"}`)
    }
    options[key.slice(2)] = value
  }
  return { command, options }
}

function requireArg(args: ParsedArgs, key: string): string {
  const value = args.options[key]
  if (!value) {
    throw new Error(`missing required argument --${key}`)
  }
  return value
}

function drainProcessStream(stream: NodeJS.WritableStream): Promise<void> {
  return new Promise((resolveDrain) => {
    stream.write("", () => resolveDrain())
  })
}

function drainProcessOutputStreams(): Promise<void> {
  return Promise.all([
    drainProcessStream(process.stdout),
    drainProcessStream(process.stderr),
  ]).then(() => undefined)
}

if (import.meta.main) {
  runAdapterCli()
    .then(async (status) => {
      process.exitCode = status
      await Promise.all([drainProcessStream(process.stdout), drainProcessStream(process.stderr)])
      process.exit(status)
    })
    .catch(async (error: unknown) => {
      process.exitCode = 1
      process.stderr.write(`${JSON.stringify({ level: "error", kind: "bad_request", message: String(error) })}\n`)
      await drainProcessStream(process.stderr)
      process.exit(1)
    })
}
