import { mkdir, readFile, rename, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { randomBytes } from "node:crypto"
import { createInterface } from "node:readline"
import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process"
import type { Turn } from "@helmr/sdk"
import { createOpencodeClient, type Config, type Event, type OpencodeClient } from "@opencode-ai/sdk/v2"
import { answerQuestions } from "./questions"
import { OpenCodeText } from "./text"

function deferred<T>() {
  let resolve!: (value: T) => void, reject!: (error: unknown) => void
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no })
  void promise.catch(() => {})
  return { promise, resolve, reject }
}

interface Operation {
  turn: Turn
  stop?: () => void
  cancel: AbortController
  failure: ReturnType<typeof deferred<never>>
  jobs: Set<Promise<void>>
  requests: Map<string, AbortController>
  seen: Set<string>
  text: OpenCodeText
  outputChain: Promise<void>
  outputBytes: number
  hasOutput: boolean
  error?: unknown
}

// This application owns the server protocol. The managed runtime separately
// owns process containment and the Session/Turn convergence boundary.
export class OpenCodeHarness {
  private client!: OpencodeClient
  private child!: ChildProcessWithoutNullStreams
  private exited = Promise.resolve()
  private readonly lifetime = new AbortController()
  private closing?: Promise<void>
  private closed = false
  private active?: Operation
  private sessionID = ""
  private constructor(private readonly cwd: string) {}
  get processId() { return this.child?.pid }
  get conversationId() { return this.sessionID }
  get idle() { return this.active === undefined }
  get reusable() { return !this.closed }

  static async open(cwd: string, helmrSessionId: string, config: Config, environment: NodeJS.ProcessEnv = process.env): Promise<OpenCodeHarness> {
    const harness = new OpenCodeHarness(cwd)
    try { await harness.initialize(helmrSessionId, config, environment); return harness }
    catch (error) { await harness.close(); throw error }
  }

  private async initialize(helmrSessionId: string, config: Config, environment: NodeJS.ProcessEnv) {
    const home = join(this.cwd, ".helmr", "opencode", encodeURIComponent(helmrSessionId))
    await mkdir(home, { recursive: true, mode: 0o700 })
    const identityFile = join(home, "conversation.json")
    let saved: string | undefined
    try {
      const value = JSON.parse(await readFile(identityFile, "utf8"))
      if (typeof value.id !== "string" || !value.id) throw new Error("Invalid saved OpenCode conversation")
      saved = value.id
    } catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error }
    const password = randomBytes(32).toString("hex")
    this.child = spawn("opencode", ["serve", "--hostname", "127.0.0.1", "--port", "0"], {
      cwd: this.cwd, stdio: "pipe",
      env: {
        ...environment, HOME: home, XDG_CONFIG_HOME: join(home, "config"), XDG_DATA_HOME: join(home, "data"), XDG_CACHE_HOME: join(home, "cache"), XDG_STATE_HOME: join(home, "state"),
        OPENCODE_SERVER_PASSWORD: password,
        OPENCODE_DISABLE_PROJECT_CONFIG: "true",
        OPENCODE_CONFIG_CONTENT: JSON.stringify({
          ...config, autoupdate: false, share: "disabled",
          experimental: { ...config.experimental, continue_loop_on_deny: true },
          permission: {
            "*": "ask", read: { "*": "allow", "*.env": "ask", "*.env.*": "ask", "*.env.example": "allow" },
            edit: { "*": "ask", ".helmr/**": "deny", "**/.helmr/**": "deny" },
            list: "allow", glob: "allow", grep: "allow", question: "allow", todowrite: "allow", task: "deny", external_directory: "deny",
          },
        }),
      },
    })
    const listening = deferred<string>()
    const lines = createInterface({ input: this.child.stdout })
    // Diagnostics may contain credentials or model data. Drain without publishing.
    this.child.stderr.resume()
    this.child.stdin.on("error", () => {})
    this.child.once("error", () => listening.reject(new Error("OpenCode could not start")))
    this.child.once("exit", () => {
      lines.close()
      this.closed = true
      const error = new Error("OpenCode server closed")
      this.lifetime.abort(error)
      listening.reject(error)
      this.active?.failure.reject(error)
    })
    this.exited = new Promise(resolve => this.child.once("close", () => resolve()))
    lines.on("line", line => {
      const match = /opencode server listening on (http:\/\/127\.0\.0\.1:\d+)/.exec(line)
      if (match) listening.resolve(match[1]!)
    })
    const timeout = setTimeout(() => listening.reject(new Error("OpenCode startup timed out")), 120_000)
    let baseUrl: string
    try { baseUrl = await listening.promise } finally { clearTimeout(timeout) }
    this.client = createOpencodeClient({ baseUrl, directory: this.cwd, headers: { Authorization: `Basic ${Buffer.from(`opencode:${password}`).toString("base64")}` } })
    const options = { throwOnError: true as const, signal: AbortSignal.any([this.lifetime.signal, AbortSignal.timeout(30_000)]) }
    const response = saved
      ? await this.client.session.get({ sessionID: saved }, options)
      : await this.client.session.create({ title: "Helmr assistant" }, options)
    this.sessionID = response.data.id
    if (saved && saved !== this.sessionID) throw new Error("OpenCode conversation identity changed")
    if (!saved) {
      await writeFile(`${identityFile}.tmp`, JSON.stringify({ id: this.sessionID }), { mode: 0o600 })
      await rename(`${identityFile}.tmp`, identityFile)
    }
  }

  run(turn: Turn, prompt: string): Promise<{ nativeSessionId: string; response?: string }> {
    let running: Promise<{ nativeSessionId: string; response?: string }>
    turn.onCancel(async () => {
      if (this.active?.turn === turn) this.active.stop?.()
      await running.catch(() => {})
      if (this.closed) throw this.lifetime.signal.reason ?? new Error("OpenCode server is closed")
      await this.assertIdle()
    })
    running = this.runTurn(turn, prompt)
    return running
  }

  private async runTurn(turn: Turn, prompt: string): Promise<{ nativeSessionId: string; response?: string }> {
    if (!this.reusable || this.active) throw new Error("OpenCode conversation is not ready")
    turn.signal.throwIfAborted()
    const cancel = new AbortController()
    const failure = deferred<never>()
    const promptID = `msg_${((BigInt(Date.now()) * 4096n) & 0xffffffffffffn).toString(16).padStart(12, "0")}${randomBytes(7).toString("hex")}`
    const operation: Operation = {
      text: new OpenCodeText(this.sessionID, promptID, text => this.publish(operation, text)), outputChain: Promise.resolve(), outputBytes: 0, hasOutput: false,
      turn, cancel, failure, jobs: new Set(), requests: new Map(), seen: new Set(),
    }
    this.active = operation
    const streamCancel = new AbortController()
    const signal = AbortSignal.any([this.lifetime.signal, streamCancel.signal])
    const connected = deferred<void>()
    let reader: Promise<void> | undefined
    let promptJob: Promise<unknown> | undefined
    let stopJob: Promise<void> | undefined
    let terminal = false
    let nativeFailed = false
    const stop = () => {
      cancel.abort(new Error("OpenCode Turn stopped"))
      failure.reject(new Error("OpenCode Turn stopped"))
      if (terminal) return
      stopJob ??= (async () => {
        try {
          await this.client.session.abort({ sessionID: this.sessionID }, { throwOnError: true, signal: this.lifetime.signal })
          await promptJob?.catch(() => {})
          await this.assertIdle()
          terminal = true
        } catch { await this.close() }
      })()
    }
    operation.stop = stop
    turn.signal.addEventListener("abort", stop, { once: true })
    try {
      const stream = await this.client.event.subscribe({}, { signal, sseMaxRetryAttempts: 1, onSseError: () => { if (!signal.aborted) failure.reject(new Error("OpenCode event connection failed")) } })
      reader = (async () => {
        for await (const event of stream.stream) {
          if (event.type === "server.connected") connected.resolve()
          else this.route(operation, event)
        }
        if (!signal.aborted) throw new Error("OpenCode event connection closed")
      })()
      void reader.catch(error => { connected.reject(error); failure.reject(error) })
      await Promise.race([connected.promise, failure.promise])
      turn.signal.throwIfAborted()
      const sending = this.client.session.prompt({
        sessionID: this.sessionID, messageID: promptID,
        parts: [{ type: "text", text: prompt }],
        system: "Help the user with the requested task. Match their language. When a material detail is missing, use the question tool and wait for their answer. Never request or reveal credentials in conversation. Work inside the workspace. Report the result clearly and briefly.",
      }, { throwOnError: true, signal: this.lifetime.signal })
      promptJob = sending
      const response = await Promise.race([sending, failure.promise])
      // The prompt HTTP response and native idle are separate facts.
      await this.assertIdle()
      terminal = true
      if (response.data.info.sessionID !== this.sessionID || response.data.info.parentID !== promptID) throw new Error("OpenCode response does not belong to this prompt")
      if (response.data.info.error) { nativeFailed = true; throw new Error("OpenCode model response failed") }
      streamCancel.abort()
      await reader
      cancel.abort(new Error("OpenCode prompt completed"))
      operation.text.finish(response.data.info.id)
      await Promise.all([...operation.jobs, operation.outputChain])
      if (operation.error !== undefined) throw operation.error
      turn.signal.throwIfAborted()
      const text = response.data.parts.filter(part => part.type === "text" && !part.synthetic && !part.ignored && part.sessionID === this.sessionID && part.messageID === response.data.info.id).map(part => part.type === "text" ? part.text : "").join("\n\n")
      return { nativeSessionId: this.sessionID, ...(text ? { response: text } : {}) }
    } catch (error) {
      if (nativeFailed && !turn.signal.aborted) operation.text.flush()
      stop()
      await stopJob
      await Promise.allSettled([...operation.jobs, ...(promptJob ? [promptJob] : [])])
      throw error
    } finally {
      cancel.abort()
      streamCancel.abort()
      await reader?.catch(() => {})
      await stopJob
      turn.signal.removeEventListener("abort", stop)
      this.active = undefined
    }
  }

  private async assertIdle() {
    const status = await this.client.session.status({}, { throwOnError: true, signal: AbortSignal.any([this.lifetime.signal, AbortSignal.timeout(10_000)]) })
    if (status.data[this.sessionID]?.type !== undefined && status.data[this.sessionID]?.type !== "idle") throw new Error("OpenCode prompt returned before idle")
  }

  private route(operation: Operation, event: Event) {
    if (operation.cancel.signal.aborted || !("sessionID" in event.properties) || event.properties.sessionID !== this.sessionID) return
    operation.text.observe(event)
    if (event.type === "question.v2.asked" || event.type === "permission.v2.asked") {
      operation.failure.reject(new Error("OpenCode returned an unsupported interactive form"))
      return
    }
    if (event.type === "question.replied" || event.type === "question.rejected" || event.type === "permission.replied") {
      operation.requests.get(event.properties.requestID)?.abort()
      return
    }
    if (event.type !== "question.asked" && event.type !== "permission.asked") return
    const key = `${event.type}:${event.properties.id}`
    if (operation.seen.has(key)) return
    operation.seen.add(key)
    const job = (async () => {
      const request = event.properties
      const requestCancel = new AbortController()
      operation.requests.set(request.id, requestCancel)
      const signal = AbortSignal.any([operation.cancel.signal, requestCancel.signal, this.lifetime.signal])
      try {
        if (event.type === "question.asked") {
          const answers = await answerQuestions(operation.turn, event.properties.questions, signal)
          await this.client.question.reply({ requestID: request.id, answers }, { throwOnError: true, signal })
        } else {
          const details = JSON.stringify({ permission: event.properties.permission, patterns: event.properties.patterns, metadata: event.properties.metadata }, null, 2)
          if (Buffer.byteLength(details) > 48 * 1024) {
            await this.client.permission.reply({ requestID: request.id, reply: "reject", message: "Too large to review in one approval; split it into smaller edits" }, { throwOnError: true, signal })
            return
          }
          const reply = await operation.turn.ask({
            prompt: [{ type: "text", text: `Allow this operation once?\n${details}` }],
             type: "choice", options: [{ id: "allow", label: "Allow once", value: true }, { id: "deny", label: "Deny", value: false }],
          }, { signal })
          signal.throwIfAborted()
          await this.client.permission.reply({ requestID: request.id, reply: reply.answer.selected.length === 1 && reply.answer.selected[0]?.value === true ? "once" : "reject" }, { throwOnError: true, signal })
        }
      } catch (error) { if (!signal.aborted) throw error }
      finally { operation.requests.delete(request.id) }
    })()
    operation.jobs.add(job)
    void job.finally(() => operation.jobs.delete(job)).catch(() => {})
    void job.catch(error => { operation.error ??= error; operation.failure.reject(error) })
  }

  private publish(operation: Operation, value: string): void {
    if (!value || operation.turn.signal.aborted) return
    const text = (operation.hasOutput ? "\n\n" : "") + value
    operation.hasOutput = true
    operation.outputBytes += Buffer.byteLength(text)
    if (operation.outputBytes > 8 * 1024 * 1024) throw new Error("OpenCode output limit exceeded")
    const job = operation.outputChain = operation.outputChain.then(() => operation.turn.output.write(text).then(() => {}))
    operation.jobs.add(job)
    void job.finally(() => operation.jobs.delete(job)).catch(() => {})
    void job.catch(error => { operation.error ??= error; operation.failure.reject(error) })
  }

  close(): Promise<void> {
    return this.closing ??= (async () => {
      this.closed = true
      this.lifetime.abort(new Error("OpenCode server is closing"))
      this.active?.cancel.abort()
      if (!this.child) return
      this.child.kill("SIGTERM")
      const kill = setTimeout(() => this.child.kill("SIGKILL"), 2_000)
      try { await this.exited } finally { clearTimeout(kill) }
    })()
  }
}
