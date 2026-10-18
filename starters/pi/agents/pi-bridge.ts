import { mkdir, readFile, stat, writeFile, rename } from "node:fs/promises"
import { join } from "node:path"
import { MessageRejected, type Turn } from "@helmr/sdk"
import { createAgentSession, createAgentSessionRuntime, type AgentSessionRuntime, DefaultResourceLoader, ModelRuntime, SessionManager, SettingsManager, type AgentSession, type CreateAgentSessionOptions } from "@earendil-works/pi-coding-agent"
import { nativeText, steeringText } from "./content"
import { questions } from "./questions"

export interface PiOptions {
  cwd: string
  stateDirectory: string
  sessionId: string
  model: string
  introduction: string
  recovery: "initial" | "reconstructed"
  native?: Omit<CreateAgentSessionOptions, "cwd" | "sessionManager" | "model">
}
interface Operation {
  turn: Turn
  cancel: AbortController
  accepting: boolean
  admissions: Set<Promise<unknown>>
  prompt?: Promise<void>
  stop?: Promise<void>
  unsubscribe?: () => void
  failure?: unknown
  response?: Extract<AgentSession["messages"][number], { role: "assistant" }>
}

export class PiSession {
  private active?: Operation
  private closed = false
  private constructor(private readonly runtime: AgentSessionRuntime) {}
  private get session() { return this.runtime.session }

  private watch(operation: Operation) {
    operation.unsubscribe?.()
    operation.response = undefined
    operation.unsubscribe = this.session.subscribe(event => {
      if (event.type === "message_end" && event.message.role === "assistant") operation.response = event.message
      if (event.type === "agent_start" && operation.cancel.signal.aborted) void this.session.abort().catch(() => {})
    })
  }

  static async open(options: PiOptions) {
    const directory = join(options.stateDirectory, encodeURIComponent(options.sessionId))
    await mkdir(directory, { recursive: true, mode: 0o700 })
    const manifest = join(directory, "session.json")
    let manager: SessionManager
    if (options.recovery === "initial") {
      manager = SessionManager.create(options.cwd, directory)
      await writeFile(manifest, JSON.stringify({ id: manager.getSessionId(), file: manager.getSessionFile() }), { mode: 0o600, flag: "wx" })
    } else {
      const saved = JSON.parse(await readFile(manifest, "utf8")) as { id: string; file: string }
      await stat(saved.file)
      manager = SessionManager.open(saved.file)
      if (manager.getSessionId() !== saved.id) throw new Error("Pi native session identity does not match saved history")
    }
    const agentDir = options.native?.agentDir ?? join(directory, "config")
    const runtime = await createAgentSessionRuntime(async target => {
      const settingsManager = options.native?.settingsManager ?? SettingsManager.create(target.cwd, target.agentDir)
      const modelRuntime = options.native?.modelRuntime ?? await ModelRuntime.create({ authPath: join(target.agentDir, "auth.json"), modelsPath: join(target.agentDir, "models.json"), modelsStorePath: join(target.agentDir, "models-store.json") })
      const slash = options.model.indexOf("/")
      const model = modelRuntime.getModel(options.model.slice(0, slash), options.model.slice(slash + 1))
      if (slash < 1 || !model) throw new Error(`Pi model is unavailable: ${options.model}`)
      const resourceLoader = options.native?.resourceLoader ?? new DefaultResourceLoader({ cwd: target.cwd, agentDir: target.agentDir, settingsManager, appendSystemPrompt: [options.introduction] })
      if (!options.native?.resourceLoader) await resourceLoader.reload()
      const created = await createAgentSession({ ...options.native, ...target, settingsManager, modelRuntime, model, resourceLoader })
      return { ...created, services: { cwd: target.cwd, agentDir: target.agentDir, settingsManager, modelRuntime, resourceLoader, diagnostics: [] }, diagnostics: [] }
    }, { cwd: options.cwd, agentDir, sessionManager: manager })
    const bridge = new PiSession(runtime)
    const replace = async <T>(action: () => Promise<T>): Promise<T> => {
      try { return await action() }
      catch (error) {
        bridge.closed = true
        if (bridge.active) bridge.active.failure = error
        throw error
      }
    }
    const bind = async () => {
      if (bridge.active) bridge.watch(bridge.active)
      await bridge.session.bindExtensions({
        mode: "rpc", uiContext: questions(bridge.session.extensionRunner.getUIContext(), () => bridge.active),
        abortHandler: () => { if (bridge.active) void bridge.stop(bridge.active).catch(() => {}) },
        onError: error => console.error("Pi extension error", error),
        commandContextActions: {
          waitForIdle: () => bridge.session.waitForIdle(),
          reload: () => bridge.session.reload(),
          navigateTree: (id, navigation) => bridge.session.navigateTree(id, navigation),
          newSession: replacement => replace(() => runtime.newSession(replacement)),
          fork: (id, replacement) => replace(() => runtime.fork(id, replacement)),
          switchSession: (path, replacement) => replace(async () => { await stat(path); return runtime.switchSession(path, replacement) }),
        },
      })
    }
    runtime.setRebindSession(async () => {
      const current = bridge.session.sessionManager
      await writeFile(`${manifest}.tmp`, JSON.stringify({ id: current.getSessionId(), file: current.getSessionFile() }), { mode: 0o600 })
      await rename(`${manifest}.tmp`, manifest)
      await bind()
    })
    try { await bind(); return bridge }
    catch (error) { await bridge.close(); throw error }
  }

  async run(turn: Turn) {
    if (this.closed || this.active) throw new Error("Pi Session is not ready")
    turn.signal.throwIfAborted()
    const operation: Operation = { turn, cancel: new AbortController(), accepting: true, admissions: new Set() }
    this.active = operation
    turn.onCancel(() => this.stop(operation))
    const cancelled = () => { void this.stop(operation).catch(() => {}) }
    turn.signal.addEventListener("abort", cancelled, { once: true })
    // prompt() can await extension input handlers before a run exists. Catch a
    // later native start too, and join the original prompt before reusing it.
    this.watch(operation)
    try {
      let admitted!: () => void
      const admission = new Promise<void>(resolve => { admitted = resolve })
      operation.prompt = this.session.prompt(nativeText(turn.input), { preflightResult: admitted })
      void operation.prompt.then(admitted, admitted)
      await turn.onMessage(async message => {
        await admission
        if (!operation.accepting || operation.cancel.signal.aborted) throw new MessageRejected("Pi input is closed")
        const messageAdmission = this.session.prompt(steeringText(message.content), { streamingBehavior: "steer" })
        operation.admissions.add(messageAdmission)
        try { await messageAdmission } finally { operation.admissions.delete(messageAdmission) }
      })
      await operation.prompt
      operation.accepting = false
      await Promise.all(operation.admissions)
      await this.session.waitForIdle()
      if (operation.failure !== undefined) throw operation.failure
      operation.cancel.signal.throwIfAborted()
      turn.signal.throwIfAborted()
      const message = operation.response
      if (message?.role === "assistant" && (message.stopReason === "error" || message.stopReason === "aborted")) throw new Error(message.errorMessage ?? "Pi execution did not succeed")
      return { nativeSessionId: this.session.sessionManager.getSessionId(), response: message?.role === "assistant" ? message.content.filter(part => part.type === "text").map(part => part.text).join("\n") : undefined }
    } catch (error) { await this.stop(operation); throw error }
    finally {
      operation.cancel.abort()
      operation.unsubscribe?.()
      turn.signal.removeEventListener("abort", cancelled)
      this.active = undefined
    }
  }

  private stop(operation: Operation): Promise<void> {
    return operation.stop ??= (async () => {
      operation.accepting = false
      operation.cancel.abort()
      try {
        this.session.clearQueue()
        await this.session.abort()
        await Promise.allSettled(operation.admissions)
        await operation.prompt?.catch(() => {})
        this.session.clearQueue()
        await this.session.abort()
      } catch (error) { this.closed = true; throw error }
    })()
  }

  async close() {
    this.closed = true
    if (this.active) await this.stop(this.active)
    this.session.clearQueue()
    await this.session.abort()
    await this.runtime.dispose()
  }
}
