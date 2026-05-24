import { create, fromBinary, toBinary, toJson } from "@bufbuild/protobuf"
import { BundleSchema, runProto } from "@helmr/proto"
import { ApprovalTimeoutError, ConcurrentWaitError, MessageTimeoutError } from "@helmr/sdk"
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
    const payload =
      error instanceof Error
        ? {
            level: "error",
            kind: classifyAdapterParseErrorKind(error),
            message: error.message,
            stack: error.stack ?? null,
          }
        : { level: "error", kind: "bad_request" as const, message: String(error) }
    io.stderr.write(`${JSON.stringify(payload)}\n`)
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
    project: config.project ?? null,
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
    const payload = parsePayload(args.options["payload-json"])
    const mintCorrelationId = createCorrelationIdMint()
    const waitGate = new WaitGate()
    const ctx = {
      wait: {
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
      run: { id: runId },
    }
    const result = await task.run(payload, ctx)
    writeTaskOutput(control, result)
  } finally {
    responses.close()
    control.close()
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
    const socketPath = process.env["HELMR_CONTROL_SOCKET"]?.trim()
    delete process.env["HELMR_CONTROL_SOCKET"]
    if (!socketPath) {
      throw new Error("HELMR_CONTROL_SOCKET is required")
    }
    return new AdapterControlWriter({ socket: await connectControlSocket(socketPath) })
  }

  readonly #target: { readonly socket: Socket } | { readonly sink: AdapterWritable }

  private constructor(target: { readonly socket: Socket } | { readonly sink: AdapterWritable }) {
    this.#target = target
  }

  write(event: runProto.RunEvent): void {
    const body = Buffer.from(toBinary(runProto.RunEventSchema, event))
    const header = Buffer.alloc(4)
    header.writeUInt32BE(body.length, 0)
    const frame = Buffer.concat([header, body])
    if ("socket" in this.#target) {
      this.#target.socket.write(frame)
    } else {
      this.#target.sink.write(frame)
    }
  }

  close(): void {
    if ("socket" in this.#target) {
      this.#target.socket.end()
    }
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

async function waitApproval(
  responses: AdapterResponseReader,
  control: AdapterControlWriter,
  mintCorrelationId: () => string,
  waitGate: WaitGate,
  message: string,
  opts?: ApprovalOptions,
): Promise<ApprovalDecision> {
  return waitGate.run(async () => waitApprovalInner(responses, control, mintCorrelationId, message, opts))
}

async function waitApprovalInner(
  responses: AdapterResponseReader,
  control: AdapterControlWriter,
  mintCorrelationId: () => string,
  message: string,
  opts?: ApprovalOptions,
): Promise<ApprovalDecision> {
  validateUtf8ByteLength("approval wait message", message, WAIT_TEXT_MAX_BYTES)
  const correlationId = mintCorrelationId()
  if (opts?.timeout !== undefined) {
    validateWaitTimeout(opts.timeout)
  }
  const policy = normalizeWaitPolicy(opts?.policy)
  control.write(create(runProto.RunEventSchema, {
    event: {
      case: "waitRequested",
      value: {
        correlationId,
        kind: {
          case: "approval",
          value: {
            message,
            ...(opts?.timeout === undefined ? {} : { timeout: opts.timeout }),
            ...(policy === undefined ? {} : { policy }),
          },
        },
      },
    },
  }))
  const decision = await responses.readDecision()
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
    at: new Date(payload.at ?? new Date().toISOString()),
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
  return waitGate.run(async () => waitMessageInner(responses, control, mintCorrelationId, prompt, opts))
}

async function waitMessageInner(
  responses: AdapterResponseReader,
  control: AdapterControlWriter,
  mintCorrelationId: () => string,
  prompt?: string,
  opts?: MessageOptions,
): Promise<MessageReply> {
  if (prompt !== undefined) {
    validateUtf8ByteLength("message wait prompt", prompt, WAIT_TEXT_MAX_BYTES)
  }
  const correlationId = mintCorrelationId()
  if (opts?.timeout !== undefined) {
    validateWaitTimeout(opts.timeout)
  }
  const policy = normalizeWaitPolicy(opts?.policy)
  control.write(create(runProto.RunEventSchema, {
    event: {
      case: "waitRequested",
      value: {
        correlationId,
        kind: {
          case: "message",
          value: {
            prompt: prompt ?? "",
            ...(opts?.timeout === undefined ? {} : { timeout: opts.timeout }),
            ...(policy === undefined ? {} : { policy }),
          },
        },
      },
    },
  }))
  const decision = await responses.readDecision()
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
    at: new Date(payload.at ?? new Date().toISOString()),
    attachments: parseAttachments(payload.attachments),
  }
}

function formatTimeoutSuffix(timeout: number | undefined): string {
  return timeout === undefined ? "" : ` after ${timeout}`
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
  readonly at?: string
  readonly principal?: string
  readonly text?: string
  readonly attachments?: unknown
}

function parseResumePayload(json: string): ResumePayload {
  if (json === "") {
    return {}
  }
  const parsed = JSON.parse(json)
  if (parsed === null || typeof parsed !== "object") {
    return {}
  }
  return parsed as ResumePayload
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

function writeTaskOutput(control: AdapterControlWriter, result: unknown): void {
  if (result === undefined) return
  const outputJson = JSON.stringify(result)
  if (outputJson === undefined) return
  control.write(create(runProto.RunEventSchema, {
    event: {
      case: "taskOutput",
      value: create(runProto.TaskOutputSchema, { outputJson }),
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

if (import.meta.main) {
  runAdapterCli().then((status) => {
    process.exitCode = status
  })
}
