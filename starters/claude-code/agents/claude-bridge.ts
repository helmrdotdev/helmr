import { spawn } from "node:child_process"
import { mkdir } from "node:fs/promises"
import { join } from "node:path"
import { query, type Query, type SDKMessage, type SDKUserMessage } from "@anthropic-ai/claude-agent-sdk"
import { MessageRejected, type InputContent, type Turn } from "@helmr/sdk"
import { askPermission } from "./questions"

function deferred<T>() {
  let resolve!: (value: T) => void, reject!: (reason: unknown) => void
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no })
  void promise.catch(() => {})
  return { promise, resolve, reject }
}

// This producer belongs to one native process. Closing it retires unsent input.
class Input implements AsyncIterable<SDKUserMessage> {
  private values: SDKUserMessage[] = []
  private wake?: () => void
  private closed = false
  push(value: SDKUserMessage): void {
    if (this.closed) throw new MessageRejected("Claude input is closed")
    this.values.push(value); this.wake?.()
  }
  close(): void { this.closed = true; this.values = []; this.wake?.() }
  async *[Symbol.asyncIterator](): AsyncIterator<SDKUserMessage> {
    while (!this.closed) {
      if (this.values.length) { yield this.values.shift()!; continue }
      await new Promise<void>(resolve => { this.wake = resolve })
      this.wake = undefined
    }
  }
}

interface Operation {
  turn: Turn
  pending: Set<string>
  done: ReturnType<typeof deferred<string | undefined>>
  abort: AbortController
  accepting: boolean
  output: Promise<unknown>
  questions: Set<Promise<unknown>>
}

export interface ClaudeOptions {
  cwd: string
  stateDirectory: string
  sessionId: string
  model: string
  appendSystemPrompt: string
  recovery: "initial" | "reconstructed"
}
export const nativeHome = (options: Pick<ClaudeOptions, "stateDirectory" | "sessionId">) => join(options.stateDirectory, encodeURIComponent(options.sessionId))

// One reader and one input stream live as long as the native conversation.
class Connection {
  private readonly input = new Input()
  private readonly native: Query
  private readonly reader: Promise<void>
  private readonly exited = deferred<void>()
  private tasks = new Set<string>()
  private tasksStopped = deferred<void>()
  private active?: Operation
  private closing?: Promise<void>
  private closed = false
  private broken?: unknown

  private constructor(private readonly options: ClaudeOptions, resume: boolean) {
    this.native = query({ prompt: this.input, options: {
      cwd: options.cwd, model: options.model,
      systemPrompt: { type: "preset", preset: "claude_code", append: options.appendSystemPrompt },
      ...(resume ? { resume: options.sessionId } : { sessionId: options.sessionId }),
      persistSession: true, permissionMode: "default",
      env: {
        ...process.env, CLAUDE_CONFIG_DIR: nativeHome(options),
        CLAUDE_CODE_RESUME_INTERRUPTED_TURN: "0",
      },
      canUseTool: (name, input, context) => {
        const operation = this.active
        if (!operation?.accepting) return Promise.resolve({ behavior: "deny", message: "No active Helmr Turn" })
        const signal = AbortSignal.any([context.signal, operation.abort.signal, operation.turn.signal])
        const answer = askPermission(operation.turn, name, input, context.title, signal)
        operation.questions.add(answer)
        void answer.finally(() => operation.questions.delete(answer)).catch(() => {})
        void answer.catch(error => { if (!signal.aborted) operation.done.reject(error) })
        return answer
      },
      spawnClaudeCodeProcess: ({ command, args, cwd, env }) => {
        const child = spawn(command, args, { cwd, env, stdio: "pipe" })
        child.stderr.on("data", chunk => process.stderr.write(chunk))
        child.once("error", error => this.fail(error))
        child.once("exit", () => { if (!this.closed) this.fail(new Error("Claude native process exited")) })
        child.once("close", () => this.exited.resolve())
        return child
      },
    } })
    this.reader = this.read()
    void this.reader.catch(error => { if (!this.closed) this.fail(error) })
  }

  static async create(options: ClaudeOptions, requireHistory: boolean): Promise<Connection> {
    const home = nativeHome(options)
    await mkdir(home, { recursive: true, mode: 0o700 })
    // Native resume owns validation of its saved conversation format.
    return new Connection(options, requireHistory)
  }
  async initialize(signal: AbortSignal): Promise<void> {
    const stop = () => { void this.close().catch(() => {}) }
    signal.addEventListener("abort", stop, { once: true })
    try {
      signal.throwIfAborted()
      await this.native.initializationResult()
      signal.throwIfAborted()
      this.assertClean()
    } finally { signal.removeEventListener("abort", stop) }
  }

  assertClean(): void { if (this.broken !== undefined) throw this.broken }
  private fail(error: unknown): void {
    this.broken ??= error
    this.active?.done.reject(error)
    this.tasksStopped.reject(error)
  }
  private submit(operation: Operation, content: InputContent): void {
    // Claude correlates at most 64 input UUIDs on one batched result.
    if (operation.pending.size >= 64) throw new MessageRejected("Claude input batch is full; retry after this Turn")
    const uuid = crypto.randomUUID()
    operation.pending.add(uuid)
    const text = content.map(part => part.text).join("\n")
    this.input.push({ type: "user", uuid, session_id: this.options.sessionId, parent_tool_use_id: null, message: { role: "user", content: text } })
  }
  steer(turn: Turn, content: InputContent): void {
    const operation = this.active
    if (!operation?.accepting || operation.turn !== turn || turn.signal.aborted) throw new MessageRejected("Claude Turn input is closed")
    // Native streaming input may be queued or folded into a running prompt.
    // Admission is not a promise of immediate steering or completed execution.
    this.submit(operation, content)
  }
  async run(turn: Turn, registration: Promise<void>): Promise<{ response?: string; nativeSessionId: string }> {
    this.assertClean()
    if (this.closed || this.active) throw new Error("Claude conversation is not ready")
    turn.signal.throwIfAborted()
    const operation: Operation = { turn, pending: new Set(), done: deferred(), abort: new AbortController(), accepting: true, output: Promise.resolve(), questions: new Set() }
    this.active = operation
    const stop = () => {
      operation.accepting = false
      operation.abort.abort(turn.signal.reason)
      operation.done.reject(turn.signal.reason ?? new Error("Claude Turn stopped"))
    }
    turn.signal.addEventListener("abort", stop, { once: true })
    try {
      this.submit(operation, turn.input)
      await registration
      const response = await operation.done.promise
      await operation.output
      turn.signal.throwIfAborted()
      return { ...(response === undefined ? {} : { response }), nativeSessionId: this.options.sessionId }
    } catch (error) {
      stop()
      await this.close().catch(() => {})
      throw error
    } finally {
      operation.accepting = false; operation.abort.abort()
      await Promise.allSettled([...operation.questions])
      turn.signal.removeEventListener("abort", stop)
      this.active = undefined
    }
  }

  private async read(): Promise<void> {
    for await (const message of this.native) this.observe(message)
    if (!this.closed) throw new Error("Claude native message stream ended")
  }
  private observe(message: SDKMessage): void {
    if (message.session_id !== this.options.sessionId) throw new Error("Claude conversation identity changed")
    if (message.type === "system") {
      if (message.subtype === "background_tasks_changed") {
        if (!this.tasks.size && message.tasks.length) this.tasksStopped = deferred<void>()
        this.tasks = new Set(message.tasks.map(task => task.task_id))
        if (!this.tasks.size) this.tasksStopped.resolve()
      }
      return
    }
    if (message.type !== "result") return
    const operation = this.active
    if (!operation?.accepting) return
    const ids = message.user_message_uuids ?? (message.user_message_uuid ? [message.user_message_uuid] : [])
    // Native background and other internal turns do not settle submitted input.
    if (!ids.some(id => operation.pending.has(id))) return
    for (const id of ids) operation.pending.delete(id)
    if (message.subtype !== "success" || message.is_error) {
      operation.accepting = false
      operation.done.reject(new Error("Claude Code could not finish this request"))
    } else if (operation.pending.size) {
      operation.output = operation.output.then(() => operation.turn.output.write(message.result))
      void operation.output.catch(error => operation.done.reject(error))
    } else {
      operation.accepting = false
      operation.done.resolve(message.result)
    }
  }

  close(): Promise<void> {
    return this.closing ??= (async () => {
      this.active?.abort.abort(new Error("Claude conversation is closing"))
      // Stop native tasks while its control channel and reader still exist.
      // A successful Turn deliberately does not take this path.
      try {
        this.assertClean()
        await bounded(async () => {
          await Promise.allSettled([...this.tasks].map(id => this.native.stopTask(id)))
          if (this.tasks.size) await this.tasksStopped.promise
        })
      } catch (error) { this.broken ??= error }
      finally {
        this.closed = true
        this.input.close()
        this.native.close()
        await this.exited.promise
        await this.reader.catch(() => {})
      }
      this.assertClean()
    })()
  }
}

export class ClaudeSession {
  private connection?: Connection
  private established = false
  private busy = false
  private failure?: unknown
  constructor(private readonly options: ClaudeOptions) {}

  run(turn: Turn): Promise<{ response?: string; nativeSessionId: string }> {
    if (this.busy) throw new Error("Claude Session already has an active Turn")
    if (this.failure !== undefined) throw this.failure
    this.busy = true
    const ready = deferred<Connection>()
    const registration = turn.onMessage(async message => {
      const connection = await ready.promise
      connection.steer(turn, message.content)
    })
    void registration.catch(() => {})
    let running: ReturnType<Connection["run"]>
    turn.onCancel(async () => {
      await running.catch(() => {})
      await this.close()
      if (this.failure !== undefined) throw this.failure
    })
    running = (async () => {
      try {
        turn.signal.throwIfAborted()
        if (!this.connection) {
          this.connection = await Connection.create(this.options, this.established || this.options.recovery === "reconstructed")
          await this.connection.initialize(turn.signal)
          this.established = true
        }
        const connection = this.connection
        const work = connection.run(turn, registration)
        ready.resolve(connection)
        return await work
      } catch (error) {
        ready.reject(error)
        await this.close().catch(() => {})
        throw error
      } finally { this.busy = false }
    })()
    return running
  }
  async close(): Promise<void> {
    const connection = this.connection
    this.connection = undefined
    try { await connection?.close() }
    catch (error) { this.failure = error; throw error }
  }
}

async function bounded(work: () => Promise<void>): Promise<void> {
  let timer: ReturnType<typeof setTimeout> | undefined
  try {
    await Promise.race([work(), new Promise<never>((_, reject) => {
      timer = setTimeout(() => reject(new Error("Claude task cleanup did not converge")), 2_000)
    })])
  } finally { clearTimeout(timer) }
}
