import { spawn } from "node:child_process"
import { MessageRejected } from "@helmr/sdk"
import { glob, stat } from "node:fs/promises"
import { join } from "node:path"
import { randomUUID } from "node:crypto"
import { query, type Query, type SDKUserMessage, type PermissionResult } from "@anthropic-ai/claude-agent-sdk"
import { z } from "zod"
import type { Json, Turn } from "@helmr/sdk"
import { Queue } from "./queue"
import { nativeText, steeringText } from "./native-content"
import type { HumanContent } from "@helmr/sdk"
import { conversation } from "./conversation"
import { askNativeApproval, askNativeQuestions, nativeQuestions, claudeQuestionInput } from "./native-questions"

type McpServers = NonNullable<Parameters<typeof query>[0]["options"]>["mcpServers"]

function deferred<T>() {
  let resolve!: (value: T) => void, reject!: (reason: unknown) => void
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no })
  void promise.catch(() => {})
  return { promise, resolve, reject }
}
interface Operation {
  readonly turn: Turn
  readonly abort: AbortController
  readonly done: ReturnType<typeof deferred<void>>
  readonly jobs: Set<Promise<void>>
  readonly tasks: Set<string>
  accepting: boolean
  running: boolean
  sawResult: boolean
  readonly queued: string[]
  failure?: unknown
  stop?: () => void
  finalText?: string
  candidate?: { messageId: string; text: string }
  readonly textBlocks: Set<string>
  outputChain: Promise<void>
  outputBytes: number
  hasOutput: boolean
  nativeFailed: boolean
  terminal: boolean
  readonly boundary: ReturnType<typeof deferred<void>>
}

// The streaming query and its sole reader live across successful Turns. This
// adapter joins SDK work; the platform owns the entire Session process tree.
export class ClaudeHarness {
  private readonly prompts = new Queue<SDKUserMessage>()
  private readonly lifetime = new AbortController()
  private readonly native: Query
  private readonly reader: Promise<void>
  private active: Operation | undefined
  private readonly messageRegistrations = new WeakMap<Turn, Promise<void>>()
  private closed = false
  private broken: Error | undefined
  private exited: Promise<void> = Promise.resolve()
  private terminate: (() => void) | undefined
  private pid: number | undefined
  private remember: () => Promise<void> = async () => {}
  private readonly started = deferred<void>()
  private constructor(cwd: string, directory: string, private readonly sessionId: string, resume: boolean, environment: NodeJS.ProcessEnv, mcpServers?: McpServers) {
    this.native = query({ prompt: this.prompts, options: {
      cwd, mcpServers, abortController: this.lifetime, permissionMode: "default", settingSources: [], includePartialMessages: true, persistSession: true,
      ...(resume ? { resume: sessionId } : { sessionId }),
      env: { ...environment, CLAUDE_CONFIG_DIR: directory, CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS: "1" },
      disallowedTools: ["Agent", "Task"],
      hooks: { PreToolUse: [{ hooks: [async input => {
        if (input.hook_event_name !== "PreToolUse") return {}
        const operation = this.active
        const background = typeof input.tool_input === "object" && input.tool_input !== null && "run_in_background" in input.tool_input && input.tool_input.run_in_background === true
        if (!operation?.accepting || !operation.running || operation.turn.signal.aborted || background || input.tool_name === "Agent" || input.tool_name === "Task") {
          if (operation?.accepting && operation.running && !operation.turn.signal.aborted) this.publish(operation, "The application declined an operation because background work is not supported.")
          return { hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: "Turn input is closed or background work is unsupported" } }
        }
        return {}
      }] }] },
      spawnClaudeCodeProcess: options => {
        const child = spawn(options.command, options.args, { cwd: options.cwd, env: options.env, signal: options.signal, stdio: "pipe" })
        this.terminate = () => { child.kill("SIGKILL") }
        child.once("spawn", () => { this.pid = child.pid; this.started.resolve() })
        child.once("error", error => { this.broken = error; this.started.reject(error) })
        child.once("exit", () => {
          if (this.closed) return
          const error = new Error("Claude process exited")
          this.broken ??= error
          this.active?.done.reject(error)
          this.active?.boundary.reject(error)
        })
        child.stderr.on("data", chunk => process.stderr.write(chunk))
        this.exited = new Promise(resolve => child.once("close", () => resolve()))
        return child
      },
      canUseTool: (toolName, input, options) => {
        // Capture ownership before any await; a late answer cannot bind to a new Turn.
        const operation = this.active
        if (!operation?.accepting) return Promise.resolve({ behavior: "deny" as const, message: "Turn input is closed" })
        const job = (async (): Promise<PermissionResult> => {
        if (toolName === "Agent" || toolName === "Task" || input.run_in_background === true) return { behavior: "deny", message: "Background work is not supported by this application" }
        const signal = AbortSignal.any([operation.turn.signal, operation.abort.signal, options.signal])
        const action = JSON.parse(JSON.stringify({ provider: "claude", toolUseID: options.toolUseID, toolName, input })) as Json
        if (toolName === "AskUserQuestion") {
          let questions, request
          try {
            request = claudeQuestionInput.parse(input)
            questions = nativeQuestions("claude", request.questions)
          } catch { return { behavior: "deny", message: "Unsupported native question" } }
          const { answers, annotations } = await askNativeQuestions(operation.turn, questions, signal)
          signal.throwIfAborted()
          return { behavior: "allow", updatedInput: { questions: request.questions, ...(request.metadata === undefined ? {} : { metadata: request.metadata }), answers: Object.fromEntries(Object.entries(answers).map(([key, values]) => [key, values.join(", ")])), annotations } }
        }
        const allow = await askNativeApproval(operation.turn, action, signal)
        signal.throwIfAborted()
        return allow ? { behavior: "allow", updatedInput: input } : { behavior: "deny", message: "User denied this operation" }
        })().catch((error): PermissionResult => {
          if (operation.abort.signal.aborted || operation.turn.signal.aborted || options.signal.aborted) return { behavior: "deny", message: "Native request ended" }
          throw error
        })
        this.track(operation, job.then(() => {}))
        return job
      },
    } })
    this.reader = this.read()
    void this.reader.catch(error => { this.broken ??= error instanceof Error ? error : new Error(String(error)); this.active?.done.reject(error); this.active?.boundary.reject(error) })
  }
  static async open(cwd: string, sessionId: string, environment: NodeJS.ProcessEnv = process.env, mcpServers?: McpServers): Promise<ClaudeHarness> {
    const saved = await conversation(cwd, sessionId, "claude")
    const nativeId = z.uuid().parse(saved.candidateId ?? randomUUID())
    let resume = saved.id !== undefined
    if (!resume && saved.candidateId) {
      // Only this application's reserved history directory is inspected. An
      // interrupted first prompt may have persisted history before its idle event.
      for await (const path of glob(`projects/**/${nativeId}.jsonl`, { cwd: saved.directory })) {
        if ((await stat(join(saved.directory, path))).size > 0) { resume = true; break }
      }
    }
    await saved.reserve(nativeId)
    const harness = new ClaudeHarness(cwd, saved.directory, nativeId, resume, environment, mcpServers)
    harness.remember = () => saved.remember(nativeId)
    try { await Promise.all([harness.native.initializationResult(), harness.started.promise]); return harness }
    catch (error) { await harness.close(); throw error }
  }
  get idle(): boolean { return this.active === undefined }
  get processId(): number | undefined { return this.pid }
  get reusable(): boolean { return !this.closed && !this.broken }
  run(turn: Turn, prompt: HumanContent): Promise<{ nativeProcessId: number; response?: string }> {
    let running: Promise<{ nativeProcessId: number; response?: string }>
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
  private async runTurn(turn: Turn, prompt: HumanContent): Promise<{ nativeProcessId: number; response?: string }> {
    this.assertUsable()
    if (this.active) throw new Error("Native conversation is not ready")
    turn.signal.throwIfAborted()
    const operation: Operation = { turn, textBlocks: new Set(), outputChain: Promise.resolve(), outputBytes: 0, hasOutput: false, nativeFailed: false,  abort: new AbortController(), done: deferred<void>(), jobs: new Set(), tasks: new Set(), accepting: true, running: false, sawResult: false, queued: [], terminal: false, boundary: deferred<void>() }
    let stopRequested = false
    const stop = () => {
      if (stopRequested) return
      stopRequested = true
      operation.accepting = false
      operation.queued.length = 0
      operation.abort.abort(turn.signal.reason)
      if (!operation.running && operation.tasks.size === 0) { operation.terminal = true; operation.boundary.resolve(); operation.done.resolve(); return }
      this.track(operation, this.native.interrupt())
      for (const id of operation.tasks) this.track(operation, this.native.stopTask(id))
    }
    operation.stop = stop
    this.active = operation
    turn.signal.addEventListener("abort", stop, { once: true })
    try {
      // Reserve the admitted input before exposing registration-time steering.
      operation.queued.push(nativeText(prompt))
      let registration = this.messageRegistrations.get(turn)
      if (!registration) {
      registration = turn.onMessage(async value => {
        const operation = this.active
        if (!operation || operation.turn !== turn) throw new MessageRejected("Native Turn input is closed")
        const text = steeringText(value.content)
        this.submit(operation, text)
      })
      this.messageRegistrations.set(turn, registration)
      }
      await registration
      turn.signal.throwIfAborted()
      if (!operation.running) this.startPrompt(operation)
      await operation.done.promise
      await Promise.allSettled([...operation.jobs])
      turn.signal.throwIfAborted()
      if (operation.failure !== undefined) throw operation.failure
      return { nativeProcessId: this.pid!, ...(operation.finalText === undefined ? {} : { response: operation.finalText }) }
    } catch (error) {
      operation.accepting = false
      operation.abort.abort(error)
      if (!operation.terminal && !this.broken) {
        stop()
        await operation.boundary.promise.catch(() => {})
      }
      if (!operation.terminal || this.broken) await this.close()
      if (operation.nativeFailed && !turn.signal.aborted && !this.broken) this.flushCandidate(operation)
      await Promise.allSettled([...operation.jobs])
      throw error
    } finally {
      operation.accepting = false
      operation.abort.abort()
      turn.signal.removeEventListener("abort", stop)
      if (this.active === operation) this.active = undefined
    }
  }
  async close(): Promise<void> {
    if (!this.closed) {
      this.closed = true
      this.lifetime.abort()
      this.prompts.end()
      this.native.close()
    }
    const kill = setTimeout(() => this.terminate?.(), 1000)
    try { await this.exited; await this.reader.catch(() => {}) }
    finally { clearTimeout(kill) }
  }
  // Steering is a follow-up within this Turn. Abort discards queued prompts;
  // accepting a callback never promises that its native work will complete.
  private submit(operation: Operation, text: string): void {
    if (this.active !== operation || !operation.accepting) throw new MessageRejected("Native Turn input is closed")
    operation.turn.signal.throwIfAborted()
    operation.queued.push(text)
    if (!operation.running) this.startPrompt(operation)
  }
  private startPrompt(operation: Operation): void {
    const text = operation.queued.shift()
    if (text === undefined) return
    operation.running = true
    operation.sawResult = false
    operation.finalText = undefined
    operation.candidate = undefined
    operation.textBlocks.clear()
    this.prompts.push({ type: "user", session_id: "", parent_tool_use_id: null, message: { role: "user", content: text } })
  }
  private flushCandidate(operation: Operation): void {
    if (operation.candidate === undefined) return
    const { text } = operation.candidate
    operation.candidate = undefined
    this.publish(operation, text)
  }
  private publish(operation: Operation, value: string): void {
    if (!value || operation.turn.signal.aborted) return
    const text = (operation.hasOutput ? "\n\n" : "") + value
    operation.hasOutput = true
    operation.outputBytes += Buffer.byteLength(text)
    if (operation.outputBytes > 8 * 1024 * 1024) throw new Error("Native output limit exceeded")
    operation.outputChain = operation.outputChain.then(() => operation.turn.output.write(text).then(() => {}))
    this.track(operation, operation.outputChain)
  }
  private track(operation: Operation, job: Promise<void>): void {
    operation.jobs.add(job)
    void job.finally(() => operation.jobs.delete(job)).catch(() => {})
    void job.catch(error => { operation.failure ??= error; operation.accepting = false; operation.abort.abort(error); operation.done.reject(error) })
  }
  private async read(): Promise<void> {
    for await (const message of this.native) {
      const operation = this.active
      if (!operation) continue
      if (message.type === "system" && message.subtype === "task_started") operation.tasks.add(message.task_id)
      if (message.type === "system" && message.subtype === "task_notification") operation.tasks.delete(message.task_id)
      if (message.type === "assistant" && message.session_id === this.sessionId && message.parent_tool_use_id === null && operation.running) {
        if (typeof message.uuid !== "string" || !message.uuid || typeof message.message.id !== "string") throw new Error("Native assistant message has no identity")
        if (operation.candidate && operation.candidate.messageId !== message.message.id) this.flushCandidate(operation)
        for (const [index, block] of message.message.content.entries()) {
          const key = `${message.uuid}:${index}`
          if (operation.textBlocks.has(key)) continue
          if (operation.textBlocks.size >= 16384) throw new Error("Native message buffer limit exceeded")
          operation.textBlocks.add(key)
          if (block.type === "text") {
            const text = (operation.candidate ? operation.candidate.text + "\n\n" : "") + block.text
            if (Buffer.byteLength(text) > 256 * 1024) throw new Error("Native message buffer limit exceeded")
            operation.candidate = { messageId: message.message.id, text }
          } else if (block.type === "tool_use") this.flushCandidate(operation)
        }
      }
      if (message.type === "result") {
        if (message.session_id !== this.sessionId) throw new Error("Native result does not belong to this conversation")
        if (!operation.running) throw new Error("Unsolicited native result")
        operation.sawResult = true
        if (message.subtype === "success" && !message.is_error) operation.finalText = message.result
        if (message.subtype !== "success" || message.is_error) { operation.nativeFailed = true; operation.failure ??= new Error("Native Turn failed") }
      }
      // A result can precede held-back results and background-agent processing.
      // Only the pinned SDK's idle boundary ends one submitted prompt. Keep
      // follow-ups in our own queue until that boundary, never the native queue.
      if (message.type === "system" && message.subtype === "session_state_changed" && message.state === "idle" && operation.running && operation.sawResult) {
        if (operation.tasks.size !== 0) throw new Error("Native query became idle with outstanding tasks")
        await this.remember()
        operation.running = false
        if (operation.accepting && operation.failure === undefined && operation.queued.length > 0) {
          if (operation.finalText !== undefined) this.publish(operation, operation.finalText)
          this.startPrompt(operation)
        }
        else { operation.accepting = false; operation.abort.abort(new Error("Native Turn completed")); operation.terminal = true; operation.boundary.resolve(); operation.done.resolve() }
      }
    }
    throw new Error("Native query ended")
  }
}
