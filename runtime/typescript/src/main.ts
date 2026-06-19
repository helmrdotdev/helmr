import { create, fromBinary, toBinary, toJson } from "@bufbuild/protobuf"
import { BundleSchema, runProto } from "@helmr/proto"
import {
  ConcurrentWaitError,
  WaitTimeoutError,
  type TaskContext,
  type TaskWorkspace,
  type ChannelInputDefinition,
  type ChannelInputHandle,
  type ChannelInputWaitOptions,
  type ChannelOutputAppendOptions,
  type ChannelOutputDefinition,
  type ChannelOutputHandle,
  type TaskSessionContext,
  type WaitpointHandle,
  type WaitpointResult,
  type WaitpointToken,
  type RuntimeWaitpointTokenCreateOptions,
  type WaitDurationInput,
  type WaitUntilInput,
  type WaitJson,
} from "@helmr/sdk"
import {
  enterRunRuntime,
  runtimeWaitOperand,
  validateChannelName,
  WaitpointResultImpl,
  parsePayloadWithSchema,
  parseTaskPayload,
  type RuntimeWaitOperand,
  type RuntimeWaitpointOptions,
} from "@helmr/sdk/internal"
import { createWriteStream, type WriteStream } from "node:fs"
import { createConnection, type Socket } from "node:net"
import { resolve } from "node:path"
import { inspect } from "node:util"

import {
  DuplicateTaskIdError,
  MissingConfigError,
  loadConfig,
  loadTaskRegistry,
  type RegisteredTask,
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
  const registry = await loadTaskRegistry(cwd)
  switch (output) {
    case "json": {
      io.stdout.write(`${JSON.stringify(serializeRegistry(registry))}\n`)
      break
    }
    case "binary": {
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
    leaveRuntime = enterRunRuntime({
      createWaitpointToken: (opts: RuntimeWaitpointTokenCreateOptions) =>
        createWaitpointToken(responses, control, opts),
      waitpoint: <TPayload>(opts: RuntimeWaitpointOptions) =>
        waitInput<TPayload>(responses, control, mintCorrelationId, waitGate, opts),
      waitAll: (operands: readonly RuntimeWaitOperand[]) =>
        waitAll(responses, control, mintCorrelationId, waitGate, operands),
      channelOutputAppend: (channel: string, payload: unknown, opts?: ChannelOutputAppendOptions) => writeChannelOutput(control, channel, payload, opts),
      waitFor: (input: WaitDurationInput) => waitFor(responses, control, mintCorrelationId, waitGate, input),
      waitUntil: (input: WaitUntilInput) => waitUntil(responses, control, mintCorrelationId, waitGate, input),
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
      session: createTaskSessionContext(taskContext.session.id, taskContext.session.workspace, responses, control, mintCorrelationId, waitGate),
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
    readonly workspace: TaskWorkspace
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
  const session = parseTaskSession(record["session"])
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
    session: Object.freeze({
      id: session.id,
      workspace: Object.freeze(session.workspace),
    }),
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

function parseTaskSession(value: unknown): { readonly id: string; readonly workspace: TaskWorkspace } {
  if (value === null || typeof value !== "object") {
    throw new Error("task context session is required")
  }
  const record = value as Record<string, unknown>
  return {
    id: readRequiredString(record, "id", "task context session.id"),
    workspace: parseTaskWorkspace(record["workspace"]),
  }
}

function readRequiredString(record: Record<string, unknown>, key: string, label: string): string {
  const value = record[key]
  if (typeof value !== "string" || value.trim() === "") {
    throw new Error(`${label} is required`)
  }
  return value
}

function serializeRegistry(registry: ReadonlyMap<string, RegisteredTask>): {
  readonly tasks: Record<string, {
    readonly originFile: string
    readonly modulePath: string
    readonly exportName: string
    readonly bundle: unknown
  }>
} {
  return {
    tasks: Object.fromEntries(
      [...registry.entries()]
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
  readonly ordinal?: number
  readonly aggregateCount?: number
}

interface RuntimeWaitOptions {
  readonly timeout?: number
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

  async readWaitpointTokenCreateResult(): Promise<runProto.WaitpointTokenCreateResult> {
    if (this.#closed) {
      throw new Error("adapter response stream closed")
    }
    const body = await this.#readFrameBody()
    return fromBinary(runProto.WaitpointTokenCreateResultSchema, body)
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

function createTaskSessionContext(
  id: string,
  workspace: TaskWorkspace,
  responses: AdapterResponseReader,
  control: AdapterControlWriter,
  mintCorrelationId: () => string,
  waitGate: WaitGate,
): TaskSessionContext {
  return Object.freeze({
    id,
    workspace,
    input(target: ChannelInputDefinition | string): ChannelInputHandle {
      const channel = channelTargetName(target)
      const schema = typeof target === "string" ? undefined : target.schema
      return Object.freeze({
        id: channel,
        wait: (waitOpts = {}) => {
          const operand = {
            type: "channel",
            channel,
            ...(schema === undefined ? {} : { schema }),
            options: waitOpts,
          } satisfies RuntimeWaitOperand
          return waitpointHandle(
            operand,
            () => waitChannelInput(responses, control, mintCorrelationId, waitGate, channel, schema, waitOpts),
          )
        },
      })
    },
    output(target: ChannelOutputDefinition | string): ChannelOutputHandle {
      const channel = channelTargetName(target)
      const schema = typeof target === "string" ? undefined : target.schema
      return Object.freeze({
        id: channel,
        append: async (payload: unknown, appendOpts?: ChannelOutputAppendOptions) => {
          const parsed = schema === undefined
            ? payload
            : await parsePayloadWithSchema(schema, payload, `channel ${JSON.stringify(channel)} payload`)
          return writeChannelOutput(control, channel, parsed, appendOpts)
        },
        pipe: async (source: AsyncIterable<unknown> | Iterable<unknown>, appendOpts?: ChannelOutputAppendOptions) => {
          for await (const item of source) {
            const parsed = schema === undefined
              ? item
              : await parsePayloadWithSchema(schema, item, `channel ${JSON.stringify(channel)} payload`)
            await writeChannelOutput(control, channel, parsed, appendOpts)
          }
        },
      })
    },
  })
}

function channelTargetName(target: { readonly id: string } | string): string {
  return validateChannelName(typeof target === "string" ? target : target.id)
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

async function createWaitpointToken(
  responses: AdapterResponseReader,
  control: AdapterControlWriter,
  opts: RuntimeWaitpointTokenCreateOptions,
): Promise<WaitpointToken> {
  const metadata = opts.metadata === undefined ? undefined : normalizeWaitMetadata(opts.metadata)
  const metadataJson = metadata === undefined ? undefined : JSON.stringify(metadata)
  const tags = opts.tags === undefined ? undefined : normalizeWaitTags(opts.tags)
  const timeoutInSeconds = opts.timeoutInSeconds === undefined ? undefined : positiveDelaySeconds(opts.timeoutInSeconds)
  control.write(create(runProto.RunEventSchema, {
    event: {
      case: "waitpointTokenCreateRequested",
      value: create(runProto.WaitpointTokenCreateRequestedSchema, {
        ...(opts.timeoutAt === undefined ? {} : { timeoutAt: opts.timeoutAt }),
        ...(timeoutInSeconds === undefined ? {} : { timeoutInSeconds }),
        ...(tags === undefined ? {} : { tags }),
        ...(metadataJson === undefined ? {} : { metadataJson }),
      }),
    },
  }))
  const result = await responses.readWaitpointTokenCreateResult()
  if (result.errorMessage !== undefined && result.errorMessage.trim() !== "") {
    throw new Error(result.errorMessage)
  }
  if (result.id.trim() === "") {
    throw new Error("waitpoint token create response id is required")
  }
  if (result.callbackUrl.trim() === "") {
    throw new Error("waitpoint token create response callback_url is required")
  }
  const resultMetadata = result.metadataJson === undefined || result.metadataJson.trim() === ""
    ? undefined
    : parseWaitpointTokenMetadata(result.metadataJson)
  const status = waitpointTokenStatus(result.status)
  return {
    id: result.id,
    callbackUrl: result.callbackUrl,
    ...(result.publicAccessToken === undefined ? {} : { publicAccessToken: result.publicAccessToken }),
    timeoutAt: result.timeoutAt ?? null,
    ...(status === undefined ? {} : { status }),
    ...(result.tags.length === 0 ? {} : { tags: result.tags }),
    ...(resultMetadata === undefined ? {} : { metadata: resultMetadata }),
  }
}

function waitpointTokenStatus(value: string | undefined): WaitpointToken["status"] | undefined {
  switch (value) {
    case "waiting":
    case "completed":
    case "timed_out":
    case "cancelled":
      return value
    case undefined:
    case "":
      return undefined
    default:
      throw new Error(`waitpoint token create response status is invalid: ${value}`)
  }
}

async function waitFor(
  responses: AdapterResponseReader,
  control: AdapterControlWriter,
  mintCorrelationId: () => string,
  waitGate: WaitGate,
  input: WaitDurationInput,
): Promise<void> {
  const seconds = waitDurationSeconds(input)
  const decision = await waitGenericDecision(responses, control, mintCorrelationId, waitGate, waitRequest(
    "timer",
    normalizeWaitDurationInput(input),
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
  input: WaitUntilInput,
): Promise<void> {
  const until = waitUntilInputDate(input)
  const seconds = Math.max(1, Math.ceil((until.getTime() - Date.now()) / 1000))
  const decision = await waitGenericDecision(responses, control, mintCorrelationId, waitGate, waitRequest(
    "timer",
    normalizeWaitUntilInput(input),
    { timeout: seconds },
  ))
  if (!(decision.kind === "timed_out" || decision.kind === "completed")) {
    throw new Error(`unexpected wait.until resume decision kind ${JSON.stringify(decision.kind)}`)
  }
  maybeWriteResumeConsumed(control, decision)
}

async function waitInput<TPayload>(
  responses: AdapterResponseReader,
  control: AdapterControlWriter,
  mintCorrelationId: () => string,
  waitGate: WaitGate,
  opts: RuntimeWaitpointOptions,
): Promise<WaitpointResultImpl<TPayload>> {
  const decision = await waitGenericDecision(responses, control, mintCorrelationId, waitGate, waitRequest(
    opts.kind ?? "token",
    waitpointData(opts),
    {
      ...(opts.timeout === undefined ? {} : { timeout: waitDurationSeconds(opts.timeout) }),
      ...(opts.metadata === undefined ? {} : { metadata: opts.metadata }),
      ...(opts.tags === undefined ? {} : { tags: opts.tags }),
    },
  ))
  if (decision.kind === "timed_out") {
    const seconds = opts.timeout === undefined ? undefined : waitDurationSeconds(opts.timeout)
    maybeWriteResumeConsumed(control, decision)
    return new WaitpointResultImpl<TPayload>(false, undefined as TPayload, new WaitTimeoutError(`waitpoint timed out${formatTimeoutSuffix(seconds)}`, seconds))
  }
  if (decision.kind !== "completed") {
    throw new Error(`unexpected waitpoint resume decision kind ${JSON.stringify(decision.kind)}`)
  }
  const data = parseResumeData(decision.dataJson)
  if (opts.schema === undefined) {
    maybeWriteResumeConsumed(control, decision)
    return new WaitpointResultImpl(true, data as TPayload)
  }
  const payload = await parsePayloadWithSchema(opts.schema, data, "waitpoint data") as TPayload
  maybeWriteResumeConsumed(control, decision)
  return new WaitpointResultImpl(true, payload)
}

async function waitChannelInput(
  responses: AdapterResponseReader,
  control: AdapterControlWriter,
  mintCorrelationId: () => string,
  waitGate: WaitGate,
  channel: string,
  schema: unknown,
  opts: {
    readonly correlationId?: string
    readonly timeout?: WaitDurationInput
    readonly metadata?: { readonly [key: string]: WaitJson }
    readonly tags?: string | readonly string[]
  } = {},
): Promise<WaitpointResult<unknown>> {
  const correlationId = normalizeOptionalCorrelationId(opts.correlationId)
  const decision = await waitGenericDecision(responses, control, mintCorrelationId, waitGate, waitRequest(
    "channel",
    {
      channel,
      ...(correlationId === undefined ? {} : { correlation_id: correlationId }),
    },
    {
      ...(opts.timeout === undefined ? {} : { timeout: waitDurationSeconds(opts.timeout) }),
      ...(opts.metadata === undefined ? {} : { metadata: opts.metadata }),
      ...(opts.tags === undefined ? {} : { tags: opts.tags }),
    },
  ))
  if (decision.kind === "timed_out") {
    const seconds = opts.timeout === undefined ? undefined : waitDurationSeconds(opts.timeout)
    maybeWriteResumeConsumed(control, decision)
    return new WaitpointResultImpl(false, undefined, new WaitTimeoutError(`channel ${JSON.stringify(channel)} wait timed out${formatTimeoutSuffix(seconds)}`, seconds))
  }
  if (decision.kind !== "completed") {
    throw new Error(`unexpected channel wait resume decision kind ${JSON.stringify(decision.kind)}`)
  }
  const envelope = channelWaitpointEnvelope(parseResumeData(decision.dataJson), channel)
  const data = schema === undefined
    ? envelope.data
    : await parsePayloadWithSchema(schema as never, envelope.data, `channel ${JSON.stringify(channel)} data`)
  maybeWriteResumeConsumed(control, decision)
  return completedWaitpointResult(data)
}

async function waitAll(
  responses: AdapterResponseReader,
  control: AdapterControlWriter,
  mintCorrelationId: () => string,
  waitGate: WaitGate,
  operands: readonly RuntimeWaitOperand[],
): Promise<readonly unknown[]> {
  if (operands.length === 0) {
    throw new Error("wait.all requires at least one operand")
  }
  const requests = operands.map(runtimeWaitOperandRequest)
  const decision = await waitGate.run(async () => {
    const correlationId = mintCorrelationId()
    const aggregateCount = requests.length
    requests.forEach((request, ordinal) => {
      control.write(waitpointRequestedEvent({ ...request, correlationId, ordinal, aggregateCount }))
    })
    return responses.readDecision()
  })
  if (decision.kind === "timed_out") {
    if (operands.length === 1 && (operands[0]?.type === "for" || operands[0]?.type === "until")) {
      maybeWriteResumeConsumed(control, decision)
      return [undefined]
    }
    maybeWriteResumeConsumed(control, decision)
    throw new WaitTimeoutError("wait.all timed out")
  }
  if (operands.length === 1 && decision.kind === "completed") {
    const operand = operands[0]
    if (operand === undefined) {
      throw new Error("wait.all operand is missing")
    }
    const result = await decodeWaitAllOperand(operand, parseResumeData(decision.dataJson))
    maybeWriteResumeConsumed(control, decision)
    return [result]
  }
  if (decision.kind !== "waitpoints") {
    throw new Error(`unexpected wait.all resume decision kind ${JSON.stringify(decision.kind)}`)
  }
  const envelope = waitAllEnvelope(parseResumeData(decision.dataJson), operands.length)
  const results = []
  for (let index = 0; index < operands.length; index += 1) {
    const operand = operands[index]
    if (operand === undefined) {
      throw new Error(`wait.all operand at index ${index} is missing`)
    }
    results.push(await decodeWaitAllOperand(operand, envelope[index]))
  }
  maybeWriteResumeConsumed(control, decision)
  return results
}

function runtimeWaitOperandRequest(operand: RuntimeWaitOperand): RuntimeWaitRequest {
  switch (operand.type) {
    case "for": {
      const seconds = waitDurationSeconds(operand.input)
      return waitRequest("timer", normalizeWaitDurationInput(operand.input), { timeout: seconds })
    }
    case "until": {
      const until = waitUntilInputDate(operand.input)
      const seconds = Math.max(1, Math.ceil((until.getTime() - Date.now()) / 1000))
      return waitRequest("timer", normalizeWaitUntilInput(operand.input), { timeout: seconds })
    }
    case "waitpoint":
      return waitRequest(
        operand.options.kind ?? "token",
        waitpointData(operand.options),
        {
          ...(operand.options.timeout === undefined ? {} : { timeout: waitDurationSeconds(operand.options.timeout) }),
          ...(operand.options.metadata === undefined ? {} : { metadata: operand.options.metadata }),
          ...(operand.options.tags === undefined ? {} : { tags: operand.options.tags }),
        },
      )
    case "channel": {
      const correlationId = normalizeOptionalCorrelationId(operand.options?.correlationId)
      return waitRequest(
        "channel",
        {
          channel: operand.channel,
          ...(correlationId === undefined ? {} : { correlation_id: correlationId }),
        },
        {
          ...(operand.options?.timeout === undefined ? {} : { timeout: waitDurationSeconds(operand.options.timeout) }),
          ...(operand.options?.metadata === undefined ? {} : { metadata: operand.options.metadata }),
          ...(operand.options?.tags === undefined ? {} : { tags: operand.options.tags }),
        },
      )
    }
  }
}

async function decodeWaitAllOperand(operand: RuntimeWaitOperand, value: unknown): Promise<unknown> {
  switch (operand.type) {
    case "for":
    case "until":
      return undefined
    case "waitpoint":
      if (operand.options.schema === undefined) {
        return value
      }
      return await parsePayloadWithSchema(operand.options.schema, value, "wait.all waitpoint data")
    case "channel": {
      const envelope = channelWaitpointEnvelope(value, operand.channel)
      if (operand.schema === undefined) {
        return envelope.data
      }
      return await parsePayloadWithSchema(operand.schema, envelope.data, `channel ${JSON.stringify(operand.channel)} data`)
    }
  }
}

function waitAllEnvelope(value: unknown, expectedLength: number): readonly unknown[] {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("wait.all data must be an object")
  }
  const waitpoints = (value as { readonly waitpoints?: unknown }).waitpoints
  if (!Array.isArray(waitpoints)) {
    throw new Error("wait.all data.waitpoints must be an array")
  }
  if (waitpoints.length !== expectedLength) {
    throw new Error(`wait.all data.waitpoints length ${waitpoints.length} did not match operand count ${expectedLength}`)
  }
  return waitpoints
}

function normalizeOptionalCorrelationId(value: ChannelInputWaitOptions["correlationId"]): string | undefined {
  if (value === undefined) {
    return undefined
  }
  const normalized = value.trim()
  return normalized === "" ? undefined : normalized
}

function waitpointHandle<TPayload>(
  operand: RuntimeWaitOperand,
  factory: () => Promise<WaitpointResult<TPayload>>,
): WaitpointHandle<TPayload> {
  let promise: Promise<WaitpointResult<TPayload>> | undefined
  const getPromise = () => {
    promise ??= factory()
    return promise
  }
  const handle: WaitpointHandle<TPayload> = {
    then<TResult1 = WaitpointResult<TPayload>, TResult2 = never>(
      onfulfilled?: ((value: WaitpointResult<TPayload>) => TResult1 | PromiseLike<TResult1>) | null,
      onrejected?: ((reason: unknown) => TResult2 | PromiseLike<TResult2>) | null,
    ): PromiseLike<TResult1 | TResult2> {
      return getPromise().then(onfulfilled, onrejected)
    },
    unwrap: async () => (await getPromise()).unwrap(),
  }
  Object.defineProperty(handle, runtimeWaitOperand, { value: operand })
  return handle
}

function completedWaitpointResult<TPayload>(data: TPayload): WaitpointResult<TPayload> {
  return new WaitpointResultImpl(true, data)
}

function channelWaitpointEnvelope(value: unknown, expectedChannel: string): { readonly channel: string; readonly sequence: number; readonly data: unknown } {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("channel waitpoint data must be an object")
  }
  const record = value as { readonly channel?: unknown; readonly sequence?: unknown; readonly data?: unknown }
  if (record.channel !== expectedChannel) {
    throw new Error(`channel waitpoint channel mismatch: expected ${JSON.stringify(expectedChannel)}`)
  }
  if (typeof record.sequence !== "number" || !Number.isInteger(record.sequence) || record.sequence < 0) {
    throw new Error("channel waitpoint sequence must be a non-negative integer")
  }
  return {
    channel: record.channel,
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
    control.write(waitpointRequestedEvent({ ...request, correlationId }))
    return responses.readDecision()
  })
}

function waitRequest(kind: string, data: WaitJson, opts?: RuntimeWaitOptions): RuntimeWaitRequest {
  const normalizedKind = normalizeWaitKind(kind)
  const timeout = opts?.timeout
  if (timeout !== undefined) {
    validateWaitTimeout(timeout)
  }
  const tags = normalizeWaitTags(opts?.tags)
  return {
    kind: normalizedKind,
    paramsJson: JSON.stringify(data),
    ...(opts?.metadata === undefined ? {} : { metadataJson: JSON.stringify(normalizeWaitMetadata(opts.metadata)) }),
    ...(tags === undefined ? {} : { tags }),
    ...(timeout === undefined ? {} : { timeout }),
  }
}

function waitpointRequestedEvent(request: RuntimeWaitRequest & { readonly correlationId: string }): runProto.RunEvent {
  const value = waitpointRequestedValue(request)
  return create(runProto.RunEventSchema, {
    event: {
      case: "waitpointRequested",
      value,
    },
  })
}

function waitpointRequestedValue(request: RuntimeWaitRequest & { readonly correlationId: string }): runProto.WaitpointRequested {
  return create(runProto.WaitpointRequestedSchema, {
    correlationId: request.correlationId,
    kind: request.kind,
    paramsJson: request.paramsJson,
    ...(request.metadataJson === undefined ? {} : { metadataJson: request.metadataJson }),
    ...(request.tags === undefined ? {} : { tags: request.tags }),
    ...(request.timeout === undefined ? {} : { timeout: request.timeout }),
    ...(request.ordinal === undefined ? {} : { ordinal: request.ordinal }),
    ...(request.aggregateCount === undefined ? {} : { aggregateCount: request.aggregateCount }),
  })
}

function maybeWriteResumeConsumed(control: AdapterControlWriter, decision: runProto.ResumeDecision): void {
  if (!decision.requireConsumedAck) {
    return
  }
  if (decision.waitpointId.trim() === "") {
    throw new Error("resume decision waitpoint_id is required")
  }
  control.write(create(runProto.RunEventSchema, {
    event: {
      case: "resumeConsumed",
      value: {
        waitpointId: decision.waitpointId,
      },
    },
  }))
}

function formatTimeoutSuffix(timeout: number | undefined): string {
  return timeout === undefined ? "" : ` after ${timeout}`
}

function waitpointData(opts: RuntimeWaitpointOptions): WaitJson {
  return opts.params === undefined ? {} : normalizeWaitJson(opts.params, "waitpoint params")
}

function normalizeOptionalIdentifier(value: string | undefined, label: string): string | undefined {
  if (value === undefined) return undefined
  const normalized = value.trim()
  if (normalized === "") {
    throw new Error(`${label} must be non-empty`)
  }
  return normalized
}

function normalizeWaitDurationInput(input: WaitDurationInput): WaitJson {
  if (typeof input === "string") {
    return { duration: input }
  }
  if (typeof input === "number") {
    return { seconds: input }
  }
  return normalizeWaitJson(input, "wait duration input")
}

function waitDurationSeconds(input: WaitDurationInput): number {
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

function normalizeWaitUntilInput(input: WaitUntilInput): WaitJson {
  if (typeof input === "string") {
    return { date: input }
  }
  if (input instanceof Date) {
    return { date: input.toISOString() }
  }
  return normalizeWaitJson(input, "wait.until input")
}

function waitUntilInputDate(input: WaitUntilInput): Date {
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

function parseWaitpointTokenMetadata(value: string): Record<string, unknown> {
  const parsed = JSON.parse(value) as unknown
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error("waitpoint token create response metadata_json must be a JSON object")
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
    throw new Error("waitpoint data is required")
  }
  try {
    return JSON.parse(json)
  } catch (error) {
    if (error instanceof Error) {
      throw new Error(`waitpoint data must be valid JSON: ${error.message}`)
    }
    throw new Error("waitpoint data must be valid JSON")
  }
}

async function writeChannelOutput(control: AdapterControlWriter, channelInput: string, payload: unknown, opts: ChannelOutputAppendOptions = {}): Promise<void> {
  const channel = validateChannelName(channelInput)
  validateUtf8ByteLength("channel", channel, CHANNEL_NAME_MAX_BYTES)
  const contentType = opts.contentType?.trim()
  if (contentType !== undefined && contentType === "") {
    throw new Error("channel output contentType must be non-empty")
  }
  const payloadJson = JSON.stringify(payload === undefined ? null : payload)
  validateUtf8ByteLength("channel output payload_json", payloadJson, RUNTIME_CONTENT_JSON_MAX_BYTES)
  const objectRefJson = opts.objectRef === undefined ? undefined : JSON.stringify(normalizeWaitJson(opts.objectRef, "channel output objectRef"))
  if (objectRefJson !== undefined) {
    validateUtf8ByteLength("channel output object_ref_json", objectRefJson, RUNTIME_CONTENT_JSON_MAX_BYTES)
  }
  control.write(create(runProto.RunEventSchema, {
    event: {
      case: "channelOutputAppended",
      value: {
        channel,
        payloadJson,
        ...(contentType === undefined ? {} : { contentType }),
        ...(objectRefJson === undefined ? {} : { objectRefJson }),
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
