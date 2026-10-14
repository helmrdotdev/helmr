import type { RuntimeMcpConnectionOptions } from "@helmr/sdk"
import { MessageRejected } from "@helmr/sdk"
import { z } from "zod"
import type { Json, Turn } from "@helmr/sdk"
import { CodexStdio, CodexRequestRejected, spawnCodex, type NativeMessage } from "./codex-stdio"
import { nativeText, steeringText } from "./native-content"
import type { HumanContent } from "@helmr/sdk"
import { conversation } from "./conversation"
import { askNativeApproval, askNativeQuestions, nativeQuestions } from "./native-questions"

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
  stop?: () => void
  hasCommentary?: boolean
  finalText?: string
  candidate?: string
  unknownPhase: boolean
  nativeFailed: boolean
  outputBytes: number
  outputChain: Promise<void>
  readonly publishedItems: Set<string>
  terminal: boolean
  readonly commands: Set<string>
  commandsDrained: ReturnType<typeof deferred<void>>
  readonly boundary: ReturnType<typeof deferred<void>>
}

// Application adapter: one native process and conversation live for the Session.
// It accounts for protocol work; the guest must independently contain native
// descendants before claiming convergence. Native terminal text alone is not proof.
export class CodexHarness {
  private readonly server: CodexStdio
  private readonly reader: Promise<void>
  private active: Operation | undefined
  private pendingStart = false
  private buffered: NativeMessage[] = []
  private readonly messageRegistrations = new WeakMap<Turn, Promise<void>>()
  private threadId = ""
  private closed = false
  private closing: Promise<void> | undefined
  private broken: Error | undefined
  private readonly started: Promise<void>
  private pid: number | undefined
  private constructor(home: string, environment?: NodeJS.ProcessEnv) {
    const child = spawnCodex(home, environment)
    this.started = new Promise((resolve, reject) => {
      child.once("spawn", () => { this.pid = child.pid; resolve() })
      child.once("error", reject)
    })
    this.server = new CodexStdio(message => this.observe(message), child)
    this.reader = this.read()
    void this.reader.catch(error => { this.broken ??= error instanceof Error ? error : new Error(String(error)); this.active?.done.reject(error); this.active?.boundary.reject(error) })
  }
  static async open(cwd: string, sessionId: string, environment?: NodeJS.ProcessEnv, mcp?: RuntimeMcpConnectionOptions): Promise<CodexHarness> {
    const saved = await conversation(cwd, sessionId, "codex")
    const harness = new CodexHarness(saved.directory, environment)
    try {
      await harness.started
      await harness.server.request("initialize", { clientInfo: { name: "helmr-issue-fixer", version: "1.0.0" }, capabilities: { experimentalApi: true } })
      await harness.server.send({ method: "initialized" })
      harness.threadId = threadResponse.parse(await harness.server.request(saved.id ? "thread/resume" : "thread/start", {
        ...(saved.id ? { threadId: saved.id } : {}), cwd, sandbox: "workspace-write", approvalPolicy: "on-request",
        // Background shell snapshots are not joined by the native Turn boundary.
        config: {
          "features.default_mode_request_user_input": true, "features.multi_agent": false, "features.shell_snapshot": false,
          ...(mcp === undefined ? {} : { "mcp_servers.helmr": { url: mcp.url, http_headers: mcp.headers } }),
        },
      })).thread.id
      await saved.remember(harness.threadId)
      return harness
    } catch (error) { await harness.close(); throw error }
  }
  get idle(): boolean { return this.active === undefined }
  get processId(): number | undefined { return this.pid }
  get reusable(): boolean { return !this.closed && !this.broken }
  run(turn: Turn, prompt: HumanContent): Promise<{ nativeThreadId: string; nativeTurnId: string; response?: string }> {
    let running: Promise<{ nativeThreadId: string; nativeTurnId: string; response?: string }>
    turn.onCancel(async () => {
      if (this.active?.turn === turn) this.active.stop?.()
      await running.catch(() => {})
      this.assertUsable()
    })
    running = this.runTurn(turn, prompt)
    void running.catch(() => {})
    return running
  }
  private assertUsable(): void {
    if (this.broken) throw this.broken
    if (this.closed) throw new Error("Native conversation is closed")
  }
  private async runTurn(turn: Turn, prompt: HumanContent): Promise<{ nativeThreadId: string; nativeTurnId: string; response?: string }> {
    this.assertUsable()
    if (this.active) throw new Error("Native conversation is not ready")
    turn.signal.throwIfAborted()
    const operation: Operation = { turn, unknownPhase: false, nativeFailed: false, outputBytes: 0, outputChain: Promise.resolve(),  done: deferred<string>(), publishedItems: new Set(), abort: new AbortController(), jobs: new Set(), requests: new Map(), resolved: new Set(), starting: deferred<{ id: string; status: string }>(), accepting: true, terminal: false, commands: new Set(), commandsDrained: deferred<void>(), boundary: deferred<void>() }
    let interruptSent = false
    let startIssued = false
    const stop = () => {
      operation.accepting = false; operation.abort.abort(turn.signal.reason)
      if (operation.terminal) return
      if (operation.nativeId && !interruptSent && !operation.terminal) { interruptSent = true; this.track(operation, this.interrupt(operation)) }
    }
    operation.stop = stop
    this.active = operation
    turn.signal.addEventListener("abort", stop, { once: true })
    try {
      let registration = this.messageRegistrations.get(turn)
      if (!registration) {
      registration = turn.onMessage(async value => {
        const operation = this.active
        if (!operation || operation.turn !== turn || !operation.accepting) throw new MessageRejected("Native Turn input is closed")
        let text: string
        try { text = steeringText(value.content) } catch { throw new MessageRejected("Valid message content or steering text is required") }
        const native = await operation.starting.promise
        if (!operation.accepting || this.active !== operation) throw new MessageRejected("Native Turn input is closed")
        try {
          await this.server.request("turn/steer", { threadId: this.threadId, expectedTurnId: native.id, input: [{ type: "text", text, text_elements: [] }] })
        } catch (error) {
          if (error instanceof CodexRequestRejected && error.code === -32600) throw new MessageRejected("Native Turn input is closed")
          throw error
        }
      })
      this.messageRegistrations.set(turn, registration)
      }
      // Register callback admission synchronously. Native startup may finish only
      // after the authored handler has returned and closed new registrations.
      this.pendingStart = true
      let native: { id: string; status: string }
      try {
        startIssued = true
        native = turnResponse.parse(await this.server.request("turn/start", { threadId: this.threadId, input: [{ type: "text", text: nativeText(prompt), text_elements: [] }] })).turn
        operation.starting.resolve(native)
      } catch (error) { operation.starting.reject(error); throw error }
      operation.nativeId = native.id
      this.pendingStart = false
      for (const message of this.buffered.splice(0)) this.route(message)
      if (turn.signal.aborted) stop()
      await registration
      const status = await operation.done.promise
      await Promise.allSettled([...operation.jobs])
      turn.signal.throwIfAborted()
      if (operation.failure !== undefined) throw operation.failure
      if (status !== "completed") throw new Error(`Native Turn ended: ${status}`)
      if (operation.commands.size) throw new Error("Native command cessation is not established")
      if (operation.unknownPhase && operation.finalText === undefined) throw new Error("Native work completed with an unsupported message phase; its side effects may already have occurred")
      const response = operation.finalText ?? operation.candidate
      return { nativeThreadId: this.threadId, nativeTurnId: native.id, ...(response === undefined ? {} : { response }) }
    } catch (error) {
      operation.accepting = false
      operation.abort.abort(error)
      if (!startIssued && !this.broken) {
        operation.terminal = true
        operation.starting.reject(error)
        operation.boundary.resolve()
      }
      // A cooperative interruption can preserve this setup-owned conversation.
      // Only a lost/uncertain protocol boundary requires destructive escalation.
      if (!operation.terminal && operation.nativeId && !this.broken) {
        stop()
        await operation.boundary.promise.catch(() => {})
      }
      if (operation.terminal && !this.broken && operation.commands.size) {
        // This setup-owned thread has exactly one active invocation. Prior work
        // cannot return while commands remain. Clean only this thread, then join
        // the exact command item receipts observed for this invocation.
        try {
          await this.server.request("thread/backgroundTerminals/clean", { threadId: this.threadId })
          if (operation.commands.size) await operation.commandsDrained.promise
        } catch { await this.close() }
      }
      if (!operation.terminal || this.broken || operation.commands.size) await this.close()
      if (operation.nativeFailed && !turn.signal.aborted && !this.broken) {
        this.flushCandidate(operation)
        if (operation.finalText !== undefined) this.publish(operation, operation.finalText)
      }
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
  }
  private observe(message: NativeMessage): void {
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
        if (message.method === "item/started" && ["commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall", "webSearch", "imageView"].includes(started.type)) this.flushCandidate(operation)
        const command = z.object({type:z.literal("commandExecution"),id:z.string(),status:z.string(),exitCode:z.number().nullable().optional()}).safeParse(params.item)
        if (command.success) {
          if (message.method === "item/completed" && (command.data.exitCode != null || ["completed", "failed", "declined"].includes(command.data.status))) { operation.commands.delete(command.data.id); if (!operation.commands.size) operation.commandsDrained.resolve() }
          else { if (!operation.commands.size) operation.commandsDrained = deferred<void>(); operation.commands.add(command.data.id) }
        }
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
      if (operation.publishedItems.has(item.id)) return
      if (operation.publishedItems.size >= 16384 || Buffer.byteLength(item.text) > 256 * 1024) throw new Error("Native message buffer limit exceeded")
      operation.publishedItems.add(item.id)
      if (item.phase != null && item.phase !== "commentary" && item.phase !== "final_answer") {
        if (!operation.unknownPhase) process.stderr.write("Unsupported native agent message phase was excluded from human output.\n")
        operation.unknownPhase = true
        return
      }
      this.flushCandidate(operation)
      if (item.phase === "final_answer") operation.finalText = item.text
      else if (item.phase === "commentary") this.publish(operation, item.text)
      else operation.candidate = item.text

    }
  }

  private flushCandidate(operation: Operation): void {
    if (operation.candidate === undefined) return
    const text = operation.candidate
    operation.candidate = undefined
    this.publish(operation, text)
  }
  private publish(operation: Operation, value: string): void {
    if (!value || operation.turn.signal.aborted) return
    const text = (operation.hasCommentary ? "\n\n" : "") + value
    operation.hasCommentary = true
    operation.outputBytes += Buffer.byteLength(text)
    if (operation.outputBytes > 8 * 1024 * 1024) throw new Error("Native output limit exceeded")
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
      const action = JSON.parse(JSON.stringify({ provider: "codex", nativeRequestId: message.id, method: message.method, params })) as Json
      let result: Json
      if (message.method === "item/tool/requestUserInput") {
        let questions
        try { questions = nativeQuestions("codex", params.questions) }
        catch {
          await this.server.send({ id: message.id, error: { code: -32602, message: "Unsupported native question" } })
          return
        }
        const { answers } = await askNativeQuestions(operation.turn, questions, signal)
        result = { answers: Object.fromEntries(Object.entries(answers).map(([id, answers]) => [id, { answers }])) }
      } else if (message.method === "item/commandExecution/requestApproval" || message.method === "item/fileChange/requestApproval") {
        if (params.availableDecisions != null && (!Array.isArray(params.availableDecisions) || !params.availableDecisions.includes("accept") || !params.availableDecisions.includes("decline"))) {
          await this.server.send({ id: message.id, error: { code: -32602, message: "One-time approval is unavailable" } })
          return
        }
        result = { decision: await askNativeApproval(operation.turn, action, signal) ? "accept" : "decline" }
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
