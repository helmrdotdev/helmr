import type { RuntimeMcpConnection } from "@helmr/sdk/mcp"
import { registerNativeOperation, type NativeResource, type NativeInvocation } from "@helmr/sdk/internal"
import { openNativeHarness } from "./native-lifecycle"
import { MessageRejected } from "@helmr/sdk"
import { z } from "zod"
import type { Json, Turn } from "@helmr/sdk"
import { CodexStdio, CodexRequestRejected, spawnManagedCodex, type NativeMessage } from "./codex-stdio"
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
  invocation?: NativeInvocation
  hasCommentary?: boolean
  finalText?: string
  readonly publishedItems: Set<string>
  terminal: boolean
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
  private broken = false
  private readonly resource: NativeResource
  private readonly started: Promise<unknown>
  private pid: number | undefined
  private constructor(home: string, resource: NativeResource, environment?: NodeJS.ProcessEnv) {
    this.resource = resource
    const native = spawnManagedCodex(resource, home, environment)
    this.started = native.ready.then(identity => { this.pid = identity.processId })
    this.server = new CodexStdio(message => this.observe(message), native.child)
    this.reader = this.read()
    void this.reader.catch(error => { this.broken = true; this.active?.done.reject(error); this.active?.boundary.reject(error) })
  }
  static open(cwd: string, sessionId: string, environment?: NodeJS.ProcessEnv, mcp?: RuntimeMcpConnection): Promise<CodexHarness> {
    return openNativeHarness(async resource => {
    const saved = await conversation(cwd, sessionId, "codex")
    const harness = new CodexHarness(saved.directory, resource, environment)
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
    })
  }
  get idle(): boolean { return this.active === undefined }
  get processId(): number | undefined { return this.pid }
  get reusable(): boolean { return !this.closed && !this.broken }
  run(turn: Turn, prompt: HumanContent): Promise<Json> {
    const running = this.runTurn(turn, prompt)
    void running.catch(() => {})
    return running
  }
  private async runTurn(turn: Turn, prompt: HumanContent): Promise<Json> {
    if (!this.reusable || this.active) throw new Error("Native conversation is not ready")
    turn.signal.throwIfAborted()
    const operation: Operation = { turn, done: deferred<string>(), publishedItems: new Set(), abort: new AbortController(), jobs: new Set(), requests: new Map(), resolved: new Set(), starting: deferred<{ id: string; status: string }>(), accepting: true, terminal: false, boundary: deferred<void>() }
    let stopTimer: ReturnType<typeof setTimeout> | undefined
    let interruptSent = false
    let startIssued = false
    const stop = () => {
      operation.accepting = false; operation.abort.abort(turn.signal.reason)
      if (operation.terminal) return
      stopTimer ??= setTimeout(() => { void this.close().catch(error => operation.done.reject(error)) }, this.resource.cooperativeStopMs)
      if (operation.nativeId && !interruptSent && !operation.terminal) { interruptSent = true; this.track(operation, this.interrupt(operation)) }
    }
    const joined = deferred<void>()
    let finished = false
    operation.invocation = registerNativeOperation(turn, this.resource, {
      done: joined.promise,
      stop: async () => { stop(); await joined.promise.catch(() => {}) },
      state: () => ({ idle: finished, reusable: this.reusable }),
    })
    this.active = operation
    turn.signal.addEventListener("abort", stop, { once: true })
    try {
      let registration = this.messageRegistrations.get(turn)
      if (!registration) {
      registration = turn.onMessage(async value => {
        const operation = this.active
        if (!operation || operation.turn !== turn || !operation.accepting) throw new MessageRejected("Native Turn input is closed")
        let text: string
        try { text = steeringText(value) } catch { throw new MessageRejected("Valid message content or steering text is required") }
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
      if (operation.finalText !== undefined) await turn.respond(operation.finalText)
      return { nativeThreadId: this.threadId, nativeTurnId: native.id }
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
      if (!operation.terminal || this.broken) await this.close()
      await Promise.allSettled([...operation.jobs])
      throw error
    } finally {
      this.pendingStart = false
      operation.accepting = false
      operation.abort.abort()
      clearTimeout(stopTimer)
      turn.signal.removeEventListener("abort", stop)
      if (this.active === operation) this.active = undefined
      finished = true
      joined.resolve()
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
    if (message.id !== undefined) { this.track(operation, operation.invocation!.run(() => this.respond(operation, message))); return }
    if (message.method === "turn/completed") {
      const params = turnResponse.extend({ threadId: z.string() }).parse(message.params)
      if (params.threadId !== this.threadId || params.turn.id !== operation.nativeId) return
      if (params.turn.error != null) operation.failure ??= new Error("Native Turn returned an error")
      operation.accepting = false
      operation.abort.abort(new Error("Native Turn completed"))
      operation.terminal = true
      operation.boundary.resolve()
      operation.done.resolve(params.turn.status)
    } else if (message.method === "item/completed" && operation.accepting) {
      const params = target.parse(message.params)
      if (params.threadId !== this.threadId || params.turnId !== operation.nativeId) return
      const item = z.object({ type: z.literal("agentMessage"), id: z.string(), text: z.string(), phase: z.enum(["commentary", "final_answer"]).nullish() }).safeParse(params.item)
      if (!item.success || operation.publishedItems.has(item.data.id)) return
      operation.publishedItems.add(item.data.id)
      if (item.data.phase === "final_answer") operation.finalText = item.data.text
      else if (item.data.phase === "commentary") {
        const text = (operation.hasCommentary ? "\n\n" : "") + item.data.text
        operation.hasCommentary = true
        this.track(operation, operation.invocation!.run(() => operation.turn.output.write(text).then(() => {})))
      }
      // Unknown-phase text is excluded; deltas never duplicate completed snapshots.
    }
  }

  private track(operation: Operation, job: Promise<void>): void {
    operation.jobs.add(job)
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
        const answers = await askNativeQuestions(operation.turn, questions, signal)
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
