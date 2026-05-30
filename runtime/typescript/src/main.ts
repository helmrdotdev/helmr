import { create, fromBinary, toBinary, toJson } from "@bufbuild/protobuf"
import { BundleSchema, runProto } from "@helmr/proto"
import {
  ApprovalTimeoutError,
  ConcurrentWaitError,
  MessageTimeoutError,
  type GitHubRefKind,
  type GitHubPullRequestMetadata,
  type GitHubTaskSource,
  type TaskSource,
  type TaskContext,
  type TaskWorkspace,
  type WaitCreateOptions,
  type WaitForInput,
  type WaitJson,
  type WaitOptions,
  type WaitResolution,
  type WaitTokenOptions,
  type WaitUntilInput,
} from "@helmr/sdk"
import { parsePayloadWithSchema, parseTaskPayload } from "@helmr/sdk/internal"
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

const CONTROL_EVENT_TYPE_MAX_BYTES = 256
const EMIT_CONTENT_JSON_MAX_BYTES = 256 * 1024
const LOG_ENTRY_MAX_BYTES = 64 * 1024
const WAIT_TEXT_MAX_BYTES = 16 * 1024
const TRUNCATED_LOG_ENTRY_MARKER = "\n...[truncated ctx.log entry]"

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

  try {
    const registry = await loadTaskRegistry(taskCwd)
    const registeredTask = lookupRegisteredTask(registry, taskId)
    const task = registeredTask.task
    const controller = new AbortController()
    const rawPayload = parsePayload(args.options["payload-json"])
    const taskContext = parseTaskContext(requireArg(args, "task-context-json"), runId, taskId)
    const mintCorrelationId = createCorrelationIdMint()
    const waitGate = new WaitGate()
    const ctx = {
      wait: {
        create: <TPayload = unknown>(input: WaitCreateOptions) =>
          waitCreate<TPayload>(responses, control, mintCorrelationId, waitGate, input),
        for: (input: WaitForInput, opts?: Omit<WaitOptions, "timeout" | "policy">) =>
          waitFor(responses, control, mintCorrelationId, waitGate, input, opts),
        until: (input: WaitUntilInput, opts?: Omit<WaitOptions, "timeout" | "policy">) =>
          waitUntil(responses, control, mintCorrelationId, waitGate, input, opts),
        token: <TPayload = unknown>(opts?: WaitTokenOptions) =>
          waitToken<TPayload>(responses, control, mintCorrelationId, waitGate, opts),
        approval: (message: string, opts?: ApprovalOptions) =>
          waitApproval(responses, control, mintCorrelationId, waitGate, message, opts),
        message: (prompt?: string, opts?: MessageOptions) =>
          waitMessage(responses, control, mintCorrelationId, waitGate, prompt, opts),
      },
      emit: (event: EmitEvent) => emitEvent(control, event),
      log: {
        info: (...values: unknown[]) => writeLog(control, "info", values),
        warn: (...values: unknown[]) => writeLog(control, "warn", values),
        error: (...values: unknown[]) => writeLog(control, "error", values),
      },
      signal: controller.signal,
      run: taskContext.run,
      task: taskContext.task,
      source: taskContext.source,
      workspace: taskContext.workspace,
    }
    let result: unknown
    const payload = task.payloadSchema === undefined ? undefined : await parseTaskPayload(task, rawPayload)
    try {
      if (task.payloadSchema === undefined) {
        result = await (task.run as (ctx: TaskContext) => unknown)(ctx)
      } else {
        result = await task.run(payload, ctx)
      }
    } catch (error: unknown) {
      const serialized = serializeError(error)
      writeSerializedError(io.stderr, serialized)
      await drainProcessOutputStreams()
      writeTaskOutcome(control, { exitCode: 1 })
      return
    }
    const outputJson = stringifyTaskOutput(result)
    await drainProcessOutputStreams()
    writeTaskOutcome(control, outputJson === undefined ? { exitCode: 0 } : { exitCode: 0, outputJson })
  } catch (error: unknown) {
    const serialized = serializeError(error)
    writeSerializedError(io.stderr, serialized)
    await drainProcessOutputStreams()
    writeTaskOutcome(control, { exitCode: 1, errorMessage: serialized.message })
  } finally {
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
  readonly run: { readonly id: string }
  readonly task: { readonly id: string }
  readonly source: TaskSource
  readonly workspace: TaskWorkspace
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
  const source = parseTaskSource(record["source"])
  const workspace = parseTaskWorkspace(record["workspace"])
  return {
    run: Object.freeze({ id: contextRunId }),
    task: Object.freeze({ id: contextTaskId }),
    source: Object.freeze(source),
    workspace: Object.freeze(workspace),
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

function parseTaskSource(value: unknown): GitHubTaskSource {
  if (value === null || typeof value !== "object") {
    throw new Error("task context source is required")
  }
  const record = value as Record<string, unknown>
  if (record["kind"] !== "github") {
    throw new Error(`task context source.kind must be "github", received ${JSON.stringify(record["kind"])}`)
  }
  const repository = readRequiredString(record, "repository", "task context source.repository")
  const requestedRef = readRequiredString(record, "requestedRef", "task context source.requestedRef")
  const resolvedSha = readRequiredString(record, "resolvedSha", "task context source.resolvedSha")
  if (!/^[0-9a-f]{40}$/i.test(resolvedSha)) {
    throw new Error("task context source.resolvedSha must be a 40-character git SHA")
  }
  let source: GitHubTaskSource = {
    kind: "github",
    repository,
    requestedRef,
    resolvedSha: resolvedSha.toLowerCase(),
  }
  const refName = readOptionalString(record, "refName")
  if (refName !== undefined) source = { ...source, refName }
  const fullRef = readOptionalString(record, "fullRef")
  if (fullRef !== undefined) source = { ...source, fullRef }
  const subpath = readOptionalString(record, "subpath")
  if (subpath !== undefined) source = { ...source, subpath }
  const defaultBranch = readOptionalString(record, "defaultBranch")
  if (defaultBranch !== undefined) source = { ...source, defaultBranch }
  if (record["refKind"] !== undefined) {
    source = { ...source, refKind: parseRefKind(record["refKind"]) }
  }
  if (record["pullRequest"] !== undefined) {
    source = { ...source, pullRequest: parsePullRequestMetadata(record["pullRequest"]) }
  }
  return source
}

function parseRefKind(value: unknown): GitHubRefKind {
  if (value === "branch" || value === "tag" || value === "sha" || value === "pull_request" || value === "unknown") {
    return value
  }
  throw new Error(`task context source.refKind is invalid: ${JSON.stringify(value)}`)
}

function parsePullRequestMetadata(value: unknown): GitHubPullRequestMetadata {
  if (value === null || typeof value !== "object") {
    throw new Error("task context source.pullRequest must be an object")
  }
  const record = value as Record<string, unknown>
  const pullNumber = record["number"]
  if (typeof pullNumber !== "number" || !Number.isInteger(pullNumber) || pullNumber <= 0) {
    throw new Error("task context source.pullRequest.number must be a positive integer")
  }
  return {
    number: pullNumber,
    baseRef: readRequiredString(record, "baseRef", "task context source.pullRequest.baseRef"),
    baseSha: readRequiredString(record, "baseSha", "task context source.pullRequest.baseSha").toLowerCase(),
    headRef: readRequiredString(record, "headRef", "task context source.pullRequest.headRef"),
    headSha: readRequiredString(record, "headSha", "task context source.pullRequest.headSha").toLowerCase(),
  }
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

function readRequiredString(record: Record<string, unknown>, key: string, label: string): string {
  const value = record[key]
  if (typeof value !== "string" || value.trim() === "") {
    throw new Error(`${label} is required`)
  }
  return value
}

function readOptionalString(record: Record<string, unknown>, key: string): string | undefined {
  const value = record[key]
  if (value === undefined) {
    return undefined
  }
  if (typeof value !== "string" || value.trim() === "") {
    throw new Error(`task context source.${key} must be a non-empty string`)
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

interface ApprovalOptions {
  readonly timeout?: number
  readonly policy?: string
}

interface ApprovalDecision {
  readonly approved: boolean
  readonly approvedBy: string
  readonly at: Date
}

interface MessageOptions {
  readonly timeout?: number
  readonly policy?: string
}

interface MessageReply {
  readonly text: string
  readonly sentBy: string
  readonly at: Date
  readonly attachments: readonly unknown[]
}

interface RuntimeWaitRequest {
  readonly kind: string
  readonly requestJson: string
  readonly displayText?: string
  readonly timeout?: number
  readonly policy?: string
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

  async #readFrameBody(): Promise<Uint8Array> {
    await this.#fill(4)
    const len = this.#buffer.readUInt32BE(0)
    this.#buffer = this.#buffer.subarray(4)
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
      throw new ConcurrentWaitError("concurrent ctx.wait.* calls are not supported in v0.1")
    }
    this.#inFlight = true
    try {
      return await fn()
    } finally {
      this.#inFlight = false
    }
  }
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

async function waitCreate<TPayload>(
  responses: AdapterResponseReader,
  control: AdapterControlWriter,
  mintCorrelationId: () => string,
  waitGate: WaitGate,
  input: WaitCreateOptions,
): Promise<WaitResolution<TPayload>> {
  const request = waitRequest(input.kind, input.request ?? {}, input)
  return waitGeneric<TPayload>(responses, control, mintCorrelationId, waitGate, request)
}

async function waitFor(
  responses: AdapterResponseReader,
  control: AdapterControlWriter,
  mintCorrelationId: () => string,
  waitGate: WaitGate,
  input: WaitForInput,
  opts?: Omit<WaitOptions, "timeout" | "policy">,
): Promise<void> {
  const seconds = waitForInputSeconds(input)
  const decision = await waitGenericDecision(responses, control, mintCorrelationId, waitGate, waitRequest(
    "delay",
    normalizeWaitForInput(input),
    { ...opts, timeout: seconds },
  ))
  if (!(decision.timedOut || decision.kind === "timed_out" || decision.kind === "completed")) {
    throw new Error(`unexpected delay resume decision kind ${JSON.stringify(decision.kind)}`)
  }
}

async function waitUntil(
  responses: AdapterResponseReader,
  control: AdapterControlWriter,
  mintCorrelationId: () => string,
  waitGate: WaitGate,
  input: WaitUntilInput,
  opts?: Omit<WaitOptions, "timeout" | "policy">,
): Promise<void> {
  const until = waitUntilInputDate(input)
  const seconds = Math.max(1, Math.ceil((until.getTime() - Date.now()) / 1000))
  const decision = await waitGenericDecision(responses, control, mintCorrelationId, waitGate, waitRequest(
    "delay",
    normalizeWaitUntilInput(input),
    { ...opts, timeout: seconds },
  ))
  if (!(decision.timedOut || decision.kind === "timed_out" || decision.kind === "completed")) {
    throw new Error(`unexpected delay resume decision kind ${JSON.stringify(decision.kind)}`)
  }
}

async function waitToken<TPayload>(
  responses: AdapterResponseReader,
  control: AdapterControlWriter,
  mintCorrelationId: () => string,
  waitGate: WaitGate,
  opts: WaitTokenOptions = {},
): Promise<TPayload> {
  const decision = await waitGenericDecision(responses, control, mintCorrelationId, waitGate, waitRequest(
    "token",
    {},
    opts,
  ))
  const timedOut = decision.timedOut || decision.kind === "timed_out"
  if (timedOut) {
    throw new Error(`token wait timed out${formatTimeoutSuffix(opts.timeout)}`)
  }
  if (decision.kind !== "completed") {
    throw new Error(`unexpected token resume decision kind ${JSON.stringify(decision.kind)}`)
  }
  const payload = parseResumePayload(decision.resolutionPayloadJson)
  const value = payload.value
  if (opts.schema === undefined) {
    return value as TPayload
  }
  return await parsePayloadWithSchema(opts.schema, value, "wait token value") as TPayload
}

async function waitApproval(
  responses: AdapterResponseReader,
  control: AdapterControlWriter,
  mintCorrelationId: () => string,
  waitGate: WaitGate,
  message: string,
  opts?: ApprovalOptions,
): Promise<ApprovalDecision> {
  validateUtf8ByteLength("approval wait message", message, WAIT_TEXT_MAX_BYTES)
  const decision = await waitGenericDecision(responses, control, mintCorrelationId, waitGate, waitRequest(
    "approval",
    { message },
    { ...opts, displayText: message },
  ))
  const timedOut = decision.timedOut || decision.kind === "timed_out"
  if (timedOut) {
    throw new ApprovalTimeoutError(`approval timed out${formatTimeoutSuffix(opts?.timeout)}`)
  }
  if (decision.kind !== "approved" && decision.kind !== "denied") {
    throw new Error(`unexpected approval resume decision kind ${JSON.stringify(decision.kind)}`)
  }
  const payload = parseResumePayload(decision.resolutionPayloadJson)
  return {
    approved: decision.kind === "approved",
    approvedBy: payload.principal ?? "operator",
    at: payload.at,
  }
}

async function waitMessage(
  responses: AdapterResponseReader,
  control: AdapterControlWriter,
  mintCorrelationId: () => string,
  waitGate: WaitGate,
  prompt?: string,
  opts?: MessageOptions,
): Promise<MessageReply> {
  if (prompt !== undefined) {
    validateUtf8ByteLength("message wait prompt", prompt, WAIT_TEXT_MAX_BYTES)
  }
  const promptText = prompt ?? ""
  const decision = await waitGenericDecision(responses, control, mintCorrelationId, waitGate, waitRequest(
    "message",
    { prompt: promptText },
    { ...opts, displayText: promptText },
  ))
  const timedOut = decision.timedOut || decision.kind === "timed_out"
  if (timedOut) {
    throw new MessageTimeoutError(`message wait timed out${formatTimeoutSuffix(opts?.timeout)}`)
  }
  if (decision.kind !== "replied") {
    throw new Error(`unexpected message resume decision kind ${JSON.stringify(decision.kind)}`)
  }
  const payload = parseResumePayload(decision.resolutionPayloadJson)
  return {
    text: payload.text ?? "",
    sentBy: payload.principal ?? "operator",
    at: payload.at,
    attachments: parseAttachments(payload.attachments),
  }
}

async function waitGeneric<TPayload>(
  responses: AdapterResponseReader,
  control: AdapterControlWriter,
  mintCorrelationId: () => string,
  waitGate: WaitGate,
  request: RuntimeWaitRequest,
): Promise<WaitResolution<TPayload>> {
  const decision = await waitGenericDecision(responses, control, mintCorrelationId, waitGate, request)
  const timedOut = decision.timedOut || decision.kind === "timed_out"
  if (timedOut) {
    throw new Error(`${request.kind} wait timed out${formatTimeoutSuffix(request.timeout)}`)
  }
  const payload = parseResumePayload(decision.resolutionPayloadJson)
  return {
    kind: decision.kind,
    payload: (payload.value === undefined ? payload.raw : payload.value) as TPayload,
    at: payload.at,
    ...(payload.principal === undefined ? {} : { principal: payload.principal }),
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
    control.write(waitRequestedEvent({ ...request, correlationId }))
    return responses.readDecision()
  })
}

function waitRequest(kind: string, request: WaitJson, opts?: WaitOptions): RuntimeWaitRequest {
  const normalizedKind = normalizeWaitKind(kind)
  const timeout = opts?.timeout
  if (timeout !== undefined) {
    validateWaitTimeout(timeout)
  }
  const policy = normalizeWaitPolicy(opts?.policy)
  const displayText = normalizeWaitDisplayText(opts?.displayText)
  return {
    kind: normalizedKind,
    requestJson: JSON.stringify(request),
    ...(displayText === undefined ? {} : { displayText }),
    ...(timeout === undefined ? {} : { timeout }),
    ...(policy === undefined ? {} : { policy }),
  }
}

function waitRequestedEvent(request: RuntimeWaitRequest & { readonly correlationId: string }): runProto.RunEvent {
  const value = waitRequestedValue(request)
  return create(runProto.RunEventSchema, {
    event: {
      case: "waitRequested",
      value,
    },
  })
}

function waitRequestedValue(request: RuntimeWaitRequest & { readonly correlationId: string }): runProto.WaitRequested {
  return create(runProto.WaitRequestedSchema, {
    correlationId: request.correlationId,
    kind: request.kind,
    requestJson: request.requestJson,
    ...(request.displayText === undefined ? {} : { displayText: request.displayText }),
    ...(request.timeout === undefined ? {} : { timeout: request.timeout }),
    ...(request.policy === undefined ? {} : { policy: request.policy }),
  })
}

function formatTimeoutSuffix(timeout: number | undefined): string {
  return timeout === undefined ? "" : ` after ${timeout}`
}

function normalizeWaitForInput(input: WaitForInput): WaitJson {
  if (typeof input === "string") {
    return { duration: input }
  }
  if (typeof input === "number") {
    return { seconds: input }
  }
  return normalizeWaitJson(input, "wait.for input")
}

function waitForInputSeconds(input: WaitForInput): number {
  if (typeof input === "number") {
    return positiveDelaySeconds(input)
  }
  if (typeof input === "string") {
    return parseDurationSeconds(input, "wait.for duration")
  }
  const seconds = input.seconds
  if (seconds !== undefined) {
    return positiveDelaySeconds(seconds)
  }
  const milliseconds = input.milliseconds
  if (milliseconds !== undefined) {
    return positiveDelaySeconds(milliseconds / 1000)
  }
  const duration = input.duration
  if (duration !== undefined) {
    return parseDurationSeconds(duration, "wait.for duration")
  }
  throw new Error("wait.for requires seconds, milliseconds, or duration")
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

function normalizeWaitDisplayText(value: string | undefined): string | undefined {
  if (value === undefined) {
    return undefined
  }
  validateUtf8ByteLength("wait display text", value, WAIT_TEXT_MAX_BYTES)
  return value
}

function normalizeWaitPolicy(value: string | undefined): string | undefined {
  if (value === undefined) {
    return undefined
  }
  const policy = value.trim()
  if (policy === "") {
    throw new Error("wait policy must be non-empty")
  }
  return policy
}

interface ResumePayload {
  readonly raw: Record<string, unknown>
  readonly at: Date
  readonly principal?: string
  readonly text?: string
  readonly value?: unknown
  readonly attachments?: unknown
}

function parseResumePayload(json: string): ResumePayload {
  if (json === "") {
    throw new Error("resume payload must be a JSON object with required at timestamp")
  }
  const parsed = JSON.parse(json)
  if (parsed === null || typeof parsed !== "object") {
    throw new Error("resume payload must be a JSON object with required at timestamp")
  }
  const record = parsed as Record<string, unknown>
  const at = parseResumePayloadAt(record["at"])
  const principal = optionalResumePayloadString(record["principal"], "principal")
  const text = optionalResumePayloadString(record["text"], "text")
  return {
    raw: record,
    at,
    ...(principal === undefined ? {} : { principal }),
    ...(text === undefined ? {} : { text }),
    ...(record["value"] === undefined ? {} : { value: record["value"] }),
    ...(record["attachments"] === undefined ? {} : { attachments: record["attachments"] }),
  }
}

function parseResumePayloadAt(value: unknown): Date {
  if (typeof value !== "string" || value.trim() === "") {
    throw new Error("resume payload at is required and must be a valid timestamp")
  }
  const at = new Date(value)
  if (Number.isNaN(at.getTime())) {
    throw new Error("resume payload at is required and must be a valid timestamp")
  }
  return at
}

function optionalResumePayloadString(value: unknown, field: string): string | undefined {
  if (value === undefined || value === null) {
    return undefined
  }
  if (typeof value !== "string") {
    throw new Error(`resume payload ${field} must be a string`)
  }
  return value
}

function parseAttachments(value: unknown): readonly unknown[] {
  if (value === undefined || value === null) {
    return []
  }
  if (!Array.isArray(value)) {
    throw new Error("message response attachments must be an array")
  }
  return value
}

interface EmitEvent {
  readonly type: string
  readonly content: readonly unknown[]
}

function emitEvent(control: AdapterControlWriter, event: EmitEvent): void {
  if (!event || typeof event !== "object" || typeof event.type !== "string") {
    throw new Error("ctx.emit requires an event with a string type")
  }
  if (!Array.isArray(event.content)) {
    throw new Error("ctx.emit requires content array")
  }
  validateUtf8ByteLength("emit event type", event.type, CONTROL_EVENT_TYPE_MAX_BYTES)
  const contentJson = JSON.stringify(event.content)
  validateUtf8ByteLength("emit event content_json", contentJson, EMIT_CONTENT_JSON_MAX_BYTES)
  control.write(create(runProto.RunEventSchema, {
    event: {
      case: "emitEvent",
      value: { type: event.type, contentJson },
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

function writeTaskOutcome(
  control: AdapterControlWriter,
  outcome: { readonly exitCode: number; readonly errorMessage?: string; readonly outputJson?: string },
): void {
  control.write(create(runProto.RunEventSchema, {
    event: {
      case: "taskOutcome",
      value: create(runProto.TaskOutcomeSchema, {
        exitCode: outcome.exitCode,
        ...(outcome.errorMessage === undefined ? {} : { errorMessage: outcome.errorMessage }),
        ...(outcome.outputJson === undefined ? {} : { outputJson: outcome.outputJson }),
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
