import { mkdir, readFile, rename, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { MessageRejected } from "@helmr/sdk"
import { z } from "zod"
import type { JsonValue, Turn } from "@helmr/sdk"
import { CodexStdio, CodexRequestRejected, spawnCodex, type NativeMessage } from "./codex-stdio"
import { nativeText, steeringText } from "./content"
import type { HumanContent } from "@helmr/sdk"

import { askApproval, askQuestions } from "./questions"

const threadResponse = z.object({ thread: z.object({ id: z.string() }) })
const turnResponse = z.object({ turn: z.object({ id: z.string(), status: z.string(), error: z.unknown().optional() }) })
const target = z.object({ threadId: z.string(), turnId: z.string() }).passthrough()
function deferred<T>() {
  let resolve!: (value: T) => void, reject!: (reason: unknown) => void
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no })
  void promise.catch(() => {})
  return { promise, resolve, reject }
}
interface Operation {
  readonly turn: Turn
  readonly done: ReturnType<typeof deferred<string>>
  readonly abort: AbortController
  readonly jobs: Set<Promise<void>>
  readonly requests: Map<string, AbortController>
  readonly resolved: Set<string>
  readonly starting: ReturnType<typeof deferred<{ id: string; status: string }>>
  nativeId?: string
  accepting: boolean
  failure?: unknown
  hasCommentary?: boolean
  finalText?: string
  candidate?: string
  nativeFailed: boolean
  outputChain: Promise<void>
  denied: boolean
  terminal: boolean
  readonly boundary: ReturnType<typeof deferred<void>>
}

// Application adapter: one native process and conversation live for the Session.
// Command receipts and explicit background cleanup precede native owner exit.
class Connection {
  private readonly server: CodexStdio
  private readonly reader: Promise<void>
  private readonly commands = new Set<string>()
  private commandsDrained = deferred<void>()
  private active: Operation | undefined
  private pendingStart = false
  private buffered: NativeMessage[] = []
  private threadId = ""
  private closed = false
  private closing: Promise<void> | undefined
  private broken: Error | undefined
  private readonly started: Promise<void>
  private constructor(home: string) {
    const child = spawnCodex(home)
    this.started = new Promise((resolve, reject) => {
      child.once("spawn", () => resolve())
      child.once("error", reject)
    })
    this.server = new CodexStdio(message => this.observe(message), child)
    this.reader = this.read()
    void this.reader.catch(error => {
      if (this.closed) return
      this.broken ??= error instanceof Error ? error : new Error(String(error))
      this.active?.done.reject(error)
      this.active?.boundary.reject(error)
      this.commandsDrained.reject(error)
    })
  }
  static async open(options: CodexOptions, requireHistory: boolean): Promise<Connection> {
    const directory = nativeHome(options.stateDirectory, options.sessionId)
    await mkdir(directory, { recursive: true, mode: 0o700 })
    const record = join(directory, "conversation.json")
    let id: string | undefined
    try { id = z.object({ id: z.string().min(1) }).parse(JSON.parse(await readFile(record, "utf8"))).id }
    catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error }
    if (requireHistory && !id) throw new Error("Saved native conversation identity is missing")
    const connection = new Connection(directory)
    try {
      await connection.started
      await connection.server.request("initialize", { clientInfo: { name: "helmr-agent", version: "1.0.0" }, capabilities: { experimentalApi: true } })
      await connection.server.send({ method: "initialized" })
      if (id) {
        // Reading is inert. A missing native transcript is an error, never a new conversation.
        await connection.server.request("thread/read", { threadId: id, includeTurns: false })
        const queuedIds: string[] = []
        let cursor: string | null = null
        do {
          const page = z.object({ data: z.array(z.object({ id: z.string() })), nextCursor: z.string().nullish() }).parse(
            await connection.server.request("thread/queue/list", { threadId: id, cursor }))
          queuedIds.push(...page.data.map(item => item.id))
          cursor = page.nextCursor ?? null
        } while (cursor !== null)
        for (const queuedSubmissionId of queuedIds) {
          const result = z.object({ deleted: z.boolean() }).parse(await connection.server.request("thread/queue/delete", { threadId: id, queuedSubmissionId }))
          if (!result.deleted) throw new Error("Could not retire native queued input")
        }
      }
      connection.threadId = threadResponse.parse(await connection.server.request(id ? "thread/resume" : "thread/start", {
        ...(id ? { threadId: id, excludeTurns: true } : {}), cwd: options.cwd, model: options.model,
        developerInstructions: options.instructions, sandbox: "workspace-write", approvalPolicy: "on-request",
        config: { "features.default_mode_request_user_input": true, "features.shell_snapshot": false },
      })).thread.id
      if (id && connection.threadId !== id) throw new Error("Native conversation changed unexpectedly")
      if (!id) {
        await writeFile(record + ".tmp", JSON.stringify({ id: connection.threadId }), { mode: 0o600 })
        await rename(record + ".tmp", record)
      }
      return connection
    } catch (error) { await connection.close(); throw error }
  }
  assertClean(): void { if (this.broken) throw this.broken }
  async steer(turn: Turn, content: import("@helmr/sdk").InputContent): Promise<void> {
    const operation = this.active
    if (!operation || operation.turn !== turn || !operation.accepting) throw new MessageRejected("Native Turn input is closed")
    const native = await operation.starting.promise
    if (!operation.accepting || this.active !== operation) throw new MessageRejected("Native Turn input is closed")
    try {
      await this.server.request("turn/steer", { threadId: this.threadId, expectedTurnId: native.id, input: [{ type: "text", text: steeringText(content), text_elements: [] }] })
    } catch (error) {
      if (error instanceof CodexRequestRejected && error.code === -32600) throw new MessageRejected("Native Turn input is closed")
      throw error
    }
  }
  private assertUsable(): void {
    if (this.broken) throw this.broken
    if (this.closed) throw new Error("Native conversation is closed")
  }
  async run(turn: Turn, prompt: HumanContent, registration: Promise<void>): Promise<{ nativeThreadId: string; nativeTurnId: string; response?: string }> {
    this.assertUsable()
    if (this.active) throw new Error("Native conversation is not ready")
    turn.signal.throwIfAborted()
    const operation: Operation = {
      turn, nativeFailed: false, denied: false,
      outputChain: Promise.resolve(), done: deferred<string>(),
      abort: new AbortController(), jobs: new Set(), requests: new Map(), resolved: new Set(),
      starting: deferred<{ id: string; status: string }>(), accepting: true, terminal: false,
      boundary: deferred<void>(),
    }
    let interruptSent = false
    const stop = () => {
      operation.accepting = false; operation.abort.abort(turn.signal.reason)
      operation.done.reject(turn.signal.reason ?? new Error("Native Turn stopped"))
      if (operation.terminal) return
      if (operation.nativeId && !interruptSent && !operation.terminal) { interruptSent = true; this.track(operation, this.interrupt(operation)) }
    }
    this.active = operation
    turn.signal.addEventListener("abort", stop, { once: true })
    try {
      this.pendingStart = true
      let native: { id: string; status: string }
      try {
        native = turnResponse.parse(await this.server.request("turn/start", { threadId: this.threadId, input: [{ type: "text", text: nativeText(prompt), text_elements: [] }] })).turn
        operation.starting.resolve(native)
      } catch (error) {
        operation.starting.reject(error)
        if (error instanceof CodexRequestRejected) { operation.terminal = true; operation.boundary.resolve() }
        throw error
      }
      operation.nativeId = native.id
      this.pendingStart = false
      for (const message of this.buffered.splice(0)) this.route(message)
      if (turn.signal.aborted) stop()
      await registration
      const status = await operation.done.promise
      await Promise.allSettled([...operation.jobs])
      turn.signal.throwIfAborted()
      if (operation.failure !== undefined) throw operation.failure
      if (status !== "completed") {
        if (status !== "interrupted" || !operation.denied) throw new Error(`Native Turn ended: ${status}`)
        await this.cleanup(operation)
        return { nativeThreadId: this.threadId, nativeTurnId: native.id, response: "The operation was not approved." }
      }
      const response = operation.finalText ?? operation.candidate
      return { nativeThreadId: this.threadId, nativeTurnId: native.id, ...(response === undefined ? {} : { response }) }
    } catch (error) {
      operation.accepting = false
      operation.abort.abort(error)
      // Cancellation owns both the native Turn and every command it started.
      // Owner exit alone does not prove that a background command has stopped.
      if (!operation.terminal && operation.nativeId) stop()
      await this.cleanup(operation).catch(() => {})
      if (operation.nativeFailed && !turn.signal.aborted && !this.broken) this.flushPending(operation)
      await Promise.allSettled([...operation.jobs])
      throw error
    } finally {
      this.pendingStart = false
      operation.accepting = false
      operation.abort.abort()
      turn.signal.removeEventListener("abort", stop)
      if (this.active === operation) this.active = undefined
    }
  }
  get reusable(): boolean { return !this.closed }
  retire(): Promise<void> { return this.cleanup() }
  private async cleanup(operation?: Operation): Promise<void> {
    try {
      await bounded(async () => {
        this.assertClean()
        if (operation) {
          if (!operation.terminal && operation.nativeId) await operation.boundary.promise
          if (!operation.terminal) throw new Error("Native Turn boundary is unknown")
        }
        if (!this.closed && (operation?.nativeId || this.commands.size)) {
          await this.server.request("thread/backgroundTerminals/clean", { threadId: this.threadId })
          if (this.commands.size) await this.commandsDrained.promise
        }
      })
    } catch (error) { this.broken = error instanceof Error ? error : new Error(String(error)) }
    await this.close()
    this.assertClean()
  }
  close(): Promise<void> {
    return this.closing ??= (async () => {
      this.closed = true
      this.active?.abort.abort(new Error("Native process is closing"))
      await this.server.close()
      await this.reader.catch(() => {})
    })()
  }
  private async read(): Promise<void> {
    for await (const message of this.server.messages) {
      if (this.pendingStart) { this.buffered.push(message); continue }
      this.route(message)
    }
    throw new Error("Native message stream ended")
  }
  private observe(message: NativeMessage): void {
    // Background terminals belong to the native thread and can outlive a
    // successful Turn. Keep their exit receipts even while no Turn is active.
    if (message.method === "item/started" || message.method === "item/completed") {
      const params = target.parse(message.params)
      if (params.threadId === this.threadId) {
        const command = z.object({ type: z.literal("commandExecution"), id: z.string(), status: z.string(), exitCode: z.number().nullable().optional() }).safeParse(params.item)
        if (command.success) {
          const ended = message.method === "item/completed" && (command.data.exitCode != null || ["completed", "failed", "declined"].includes(command.data.status))
          if (ended) {
            this.commands.delete(command.data.id)
            if (!this.commands.size) this.commandsDrained.resolve()
          } else {
            if (!this.commands.size) this.commandsDrained = deferred<void>()
            this.commands.add(command.data.id)
          }
        }
      }
    }
    if (message.method !== "serverRequest/resolved") return
    const params = z.object({ threadId: z.string(), requestId: z.union([z.string(), z.number()]) }).parse(message.params)
    const operation = this.active
    if (!operation || params.threadId !== this.threadId) return
    const key = JSON.stringify(params.requestId)
    operation.resolved.add(key)
    operation.requests.get(key)?.abort(new Error("Native request resolved"))
  }
  private route(message: NativeMessage): void {
    const operation = this.active
    if (!operation) {
      if (message.id !== undefined) void this.server.send({ id: message.id, error: { code: -32600, message: "No active Turn" } }).catch(() => {})
      return
    }
    if (message.id !== undefined) { this.track(operation, this.respond(operation, message)); return }
    if (message.method === "item/started" || message.method === "item/completed") {
      const params = target.parse(message.params)
      if (params.threadId === this.threadId && params.turnId === operation.nativeId) {
        const started = z.object({ type: z.string() }).parse(params.item)
        if (message.method === "item/started" && ["commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall", "webSearch", "imageView"].includes(started.type)) this.flushPending(operation)
      }
    }
    if (message.method === "turn/completed") {
      const params = turnResponse.extend({ threadId: z.string() }).parse(message.params)
      if (params.threadId !== this.threadId || params.turn.id !== operation.nativeId) return
      operation.nativeFailed = params.turn.error != null || params.turn.status !== "completed"
      if (params.turn.error != null) operation.failure ??= new Error("Native Turn returned an error")
      operation.accepting = false
      operation.abort.abort(new Error("Native Turn completed"))
      operation.terminal = true
      operation.boundary.resolve()
      operation.done.resolve(params.turn.status)
    } else if (message.method === "item/completed" && operation.accepting) {
      const params = target.parse(message.params)
      if (params.threadId !== this.threadId || params.turnId !== operation.nativeId) return
      const shape = z.object({ type: z.string() }).parse(params.item)
      if (shape.type !== "agentMessage") return
      const item = z.object({ type: z.literal("agentMessage"), id: z.string(), text: z.string(), phase: z.string().nullish(), delivery: z.unknown().optional(), questions: z.unknown().optional() }).parse(params.item)
      if (item.delivery === "async" || item.questions != null) {
        this.track(operation, Promise.reject(new Error("Detached asynchronous questions are unsupported by this turn-bound bridge")))
        return
      }
      this.flushPending(operation)
      if (item.phase === "final_answer") operation.finalText = item.text
      else if (item.phase != null) this.publish(operation, item.text)
      else operation.candidate = item.text

    }
  }

  private flushPending(operation: Operation): void {
    const text = operation.finalText ?? operation.candidate
    operation.finalText = undefined
    operation.candidate = undefined
    if (text !== undefined) this.publish(operation, text)
  }

  private publish(operation: Operation, value: string): void {
    if (!value || operation.turn.signal.aborted) return
    const text = (operation.hasCommentary ? "\n\n" : "") + value
    operation.hasCommentary = true
    operation.outputChain = operation.outputChain.then(() => operation.turn.output.write(text).then(() => {}))
    this.track(operation, operation.outputChain)
  }

  private track(operation: Operation, job: Promise<void>): void {
    operation.jobs.add(job)
    void job.finally(() => operation.jobs.delete(job)).catch(() => {})
    void job.catch(error => {
      operation.failure ??= error
      operation.accepting = false
      operation.abort.abort(error)
      operation.done.reject(error)
    })
  }
  private interrupt(operation: Operation): Promise<void> {
    return this.server.request("turn/interrupt", { threadId: this.threadId, turnId: operation.nativeId }).then(() => {})
  }
  private async respond(operation: Operation, message: NativeMessage): Promise<void> {
    const key = JSON.stringify(message.id)
    const parsedTarget = target.safeParse(message.params)
    if (!parsedTarget.success || parsedTarget.data.threadId !== this.threadId || parsedTarget.data.turnId !== operation.nativeId) {
      await this.server.send({ id: message.id, error: { code: -32600, message: "Native request does not belong to this Turn" } })
      return
    }
    const params = parsedTarget.data
    if (operation.resolved.has(key) || operation.abort.signal.aborted) return
    const cancel = new AbortController()
    operation.requests.set(key, cancel)
    const signal = AbortSignal.any([operation.turn.signal, operation.abort.signal, cancel.signal])
    try {
      const action = JSON.parse(JSON.stringify({ provider: "codex", nativeRequestId: message.id, method: message.method, params })) as JsonValue
      let result: JsonValue
      if (message.method === "item/tool/requestUserInput") {
        result = await askQuestions(operation.turn, params.questions, signal)
      } else if (message.method === "item/commandExecution/requestApproval" || message.method === "item/fileChange/requestApproval") {
        if (params.availableDecisions != null && (!Array.isArray(params.availableDecisions) || !params.availableDecisions.includes("accept") || !params.availableDecisions.some(decision => decision === "decline" || decision === "cancel"))) {
          await this.server.send({ id: message.id, error: { code: -32602, message: "One-time approval is unavailable" } })
          return
        }
        const approval = await askApproval(operation.turn, action, signal, Array.isArray(params.availableDecisions) && !params.availableDecisions.includes("decline") ? "cancel" : "decline")
        operation.denied ||= approval.decision !== "accept"
        result = approval
      } else {
        await this.server.send({ id: message.id, error: { code: -32601, message: "Unsupported interactive request" } })
        return
      }
      signal.throwIfAborted()
      await this.server.send({ id: message.id, result })
    } catch (error) { if (!signal.aborted) throw error }
    finally { operation.requests.delete(key) }
  }
}

export interface CodexOptions { cwd: string; stateDirectory: string; sessionId: string; model: string; instructions: string; recovery: "initial" | "reconstructed" }
export function nativeHome(stateDirectory: string, sessionId: string): string {
  // Keep native configuration and identity outside the tools' writable workspace.
  return join(stateDirectory, encodeURIComponent(sessionId))
}

// One instance belongs to Helmr initResult. Normal Turns share its native owner;
// cancelled Turns close it and the next Turn reopens the saved native conversation.
export class CodexSession {
  private connection?: Connection
  private busy = false
  private established = false
  private failure?: unknown
  constructor(private readonly options: CodexOptions) {}

  run(turn: Turn): Promise<{ nativeThreadId: string; nativeTurnId: string; response?: string }> {
    if (this.busy) throw new Error("Codex Session already has an active Turn")
    if (this.failure) throw this.failure
    this.busy = true
    const ready = deferred<Connection>()
    // Register callbacks before the first await, including when opening an owner.
    const registration = turn.onMessage(async message => {
      const connection = await ready.promise
      turn.signal.throwIfAborted()
      await connection.steer(turn, message.content)
    })
    void registration.catch(() => {})
    let running: Promise<{ nativeThreadId: string; nativeTurnId: string; response?: string }>
    turn.onCancel(async () => {
      await running.catch(() => {})
      await this.close()
      if (this.failure) throw this.failure
    })
    running = (async () => {
      try {
        turn.signal.throwIfAborted()
        const connection = this.connection ??= await Connection.open(this.options, this.established || this.options.recovery === "reconstructed")
        this.established = true
        turn.signal.throwIfAborted()
        const work = connection.run(turn, turn.input, registration)
        ready.resolve(connection)
        const result = await work
        if (!connection.reusable) this.connection = undefined
        return result
      } catch (error) {
        ready.reject(error)
        try { this.connection?.assertClean() }
        catch (cleanupError) { this.failure = cleanupError }
        await this.close().catch(() => {})
        throw error
      } finally { this.busy = false }
    })()
    return running
  }
  async close(): Promise<void> {
    const connection = this.connection
    this.connection = undefined
    try { await connection?.retire() }
    catch (error) { this.failure = error; throw error }
  }
}

async function bounded(work: () => Promise<void>): Promise<void> {
  let timer: ReturnType<typeof setTimeout> | undefined
  try {
    await Promise.race([work(), new Promise<never>((_, reject) => {
      timer = setTimeout(() => reject(new Error("Codex cleanup did not converge within 5 seconds")), 5_000)
    })])
  } finally { clearTimeout(timer) }
}
