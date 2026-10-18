import { mkdir, stat } from "node:fs/promises"
import { join } from "node:path"
import { MessageRejected, type Turn } from "@helmr/sdk"
import { OpenCode } from "@opencode/sdk/effect"
import { Bus } from "@opencode/core/bus"
import { Session } from "@opencode/core/session"
import { SessionExecution } from "@opencode/core/session/execution"
import { SessionStore } from "@opencode/core/session/store"
import { Form } from "@opencode/schema/form"
import { Permission } from "@opencode/schema/permission"
import { Location } from "@opencode/schema/location"
import { Model } from "@opencode/schema/model"
import { Context, Effect, Exit, Layer, Schema, Scope } from "effect"
import { nativeText, steeringText } from "./content"
import { askForm, askPermission } from "./questions"
import { recovery } from "./recovery"

export interface OpenCodeOptions {
  cwd: string
  stateDirectory: string
  sessionId: string
  model: string
  introduction: string
  recovery: "initial" | "reconstructed"
  // The editable application may supply native configuration and plugins.
  native?: OpenCode.CreateOptions
}
interface Operation {
  turn: Turn
  cancel: AbortController
  jobs: Set<Promise<void>>
  seen: Set<string>
  requests: Map<string, AbortController>
  failure?: unknown
  accepting: boolean
  admission: Promise<unknown>
  stop?: Promise<void>
}

export class OpenCodeSession {
  private sdk!: OpenCode.Interface
  private sessions!: Session.Interface
  private execution!: SessionExecution.Interface
  private store!: SessionStore.Interface
  private bus!: Bus.Interface
  private scope!: Scope.Closeable
  private unsubscribe?: Bus.Unsubscribe
  private active?: Operation
  private closed = false
  private hostOpen = false
  readonly nativeSessionId: Session.ID
  private constructor(private readonly options: OpenCodeOptions) {
    this.nativeSessionId = Schema.decodeUnknownSync(Session.ID)(`ses_${options.sessionId}`)
  }

  static async open(options: OpenCodeOptions) {
    const bridge = new OpenCodeSession(options)
    try { await bridge.initialize(); return bridge }
    catch (error) { await bridge.close(); throw error }
  }

  private async initialize(kind = this.options.recovery) {
    const directory = join(this.options.stateDirectory, encodeURIComponent(this.options.sessionId))
    const path = join(directory, "session.sqlite")
    if (kind !== "initial") await stat(path)
    await mkdir(directory, { recursive: true, mode: 0o700 })
    this.scope = await Effect.runPromise(Scope.make())
    const resumed = recovery()
    this.sdk = await Effect.runPromise(OpenCode.create({ ...this.options.native, database: { path } }, {
      overrides: [resumed.replacement,
        Session.node.replace(Session.node.mapLayer(layer => Layer.tap(layer, context => Effect.sync(() => { this.sessions = Context.get(context, Session.Service) })))),
        SessionExecution.node.replace(SessionExecution.node.mapLayer(layer => Layer.tap(layer, context => Effect.sync(() => { this.execution = Context.get(context, SessionExecution.Service) })))),
        SessionStore.node.replace(SessionStore.node.mapLayer(layer => Layer.tap(layer, context => Effect.sync(() => { this.store = Context.get(context, SessionStore.Service) })))),
        Bus.node.replace(Bus.node.mapLayer(layer => Layer.tap(layer, context => Effect.sync(() => { this.bus = Context.get(context, Bus.Service) })))),
      ],
    }).pipe(Effect.provideService(Scope.Scope, this.scope)))
    this.hostOpen = true
    await resumed.ready
    if (kind !== "initial") await Effect.runPromise(this.sessions.get(this.nativeSessionId))
    else {
      const slash = this.options.model.indexOf("/")
      if (slash < 1) throw new Error("OpenCode model must include its provider")
      await Effect.runPromise(this.sdk.sessions.create({ id: this.nativeSessionId,
        location: Schema.decodeUnknownSync(Location.Ref)({ directory: this.options.cwd }),
        model: Schema.decodeUnknownSync(Model.Ref)({ providerID: this.options.model.slice(0, slash), id: this.options.model.slice(slash + 1) }),
      }))
    }
    await Effect.runPromise(this.sdk.sessions.instructions.entry.put({ sessionID: this.nativeSessionId, key: "helmr-introduction", value: this.options.introduction }))
    this.unsubscribe = await Effect.runPromise(this.bus.listen(event => Effect.sync(() => {
      if (Schema.is(Form.Event.Created)(event)) this.form(event.data.form)
      else if (Schema.is(Permission.Event.Asked)(event)) this.permission(event.data)
      else if (Schema.is(Form.Event.Replied)(event) || Schema.is(Form.Event.Cancelled)(event)) this.active?.requests.get(event.data.id)?.abort()
      else if (Schema.is(Permission.Event.Replied)(event)) this.active?.requests.get(event.data.requestID)?.abort()
    })))
  }

  private dialog(id: string, run: (operation: Operation, signal: AbortSignal) => Promise<void>) {
    const operation = this.active
    // Native background work can ask between Turns. Its pending request remains
    // native-owned and is presented when the next Helmr Turn opens.
    if (!operation || operation.cancel.signal.aborted || operation.seen.has(id)) return
    operation.seen.add(id)
    const request = new AbortController()
    const signal = AbortSignal.any([operation.cancel.signal, request.signal])
    operation.requests.set(id, request)
    const job = run(operation, signal).catch(error => {
      if (!signal.aborted) {
        operation.failure ??= error
        void this.stop(operation).catch(() => {})
      }
    })
    operation.jobs.add(job)
    void job.finally(() => { operation.jobs.delete(job); operation.requests.delete(id) })
  }
  private form(form: Form.Info) {
    this.dialog(form.id, async (operation, signal) => {
      const answer = await askForm(operation.turn, form, signal)
      await Effect.runPromise(this.sdk.sessions.form.reply({ sessionID: form.sessionID, formID: form.id, answer }))
    })
  }
  private permission(request: Permission.Request) {
    this.dialog(request.id, async (operation, signal) => {
      const decision = await askPermission(operation.turn, request, signal)
      await Effect.runPromise(this.sdk.permission.reply({ sessionID: request.sessionID, requestID: request.id, decision }))
    })
  }

  async run(turn: Turn) {
    if (this.closed || this.active) throw new Error("OpenCode Session is not ready")
    turn.signal.throwIfAborted()
    if (!this.hostOpen) {
      try { await this.initialize("reconstructed") }
      catch (error) { await this.close(); throw error }
    }
    turn.signal.throwIfAborted()
    const operation: Operation = { turn, cancel: new AbortController(), jobs: new Set(), seen: new Set(), requests: new Map(), accepting: true, admission: Promise.resolve() }
    this.active = operation
    turn.onCancel(() => this.stop(operation))
    const cancelled = () => { void this.stop(operation).catch(() => {}) }
    turn.signal.addEventListener("abort", cancelled, { once: true })
    try {
      for (const session of await Effect.runPromise(this.store.list())) {
        operation.cancel.signal.throwIfAborted()
        for (const form of await Effect.runPromise(this.sdk.sessions.form.list({ sessionID: session.id }))) this.form(form)
        for (const request of await Effect.runPromise(this.sdk.permission.list({ sessionID: session.id }))) this.permission(request)
      }
      if (operation.failure !== undefined) throw operation.failure
      operation.cancel.signal.throwIfAborted()
      turn.signal.throwIfAborted()
      const prompt = Effect.runPromise(this.sdk.sessions.prompt({ sessionID: this.nativeSessionId, text: nativeText(turn.input) }))
      operation.admission = prompt
      const admitted = await prompt
      await turn.onMessage(async message => {
        if (this.active !== operation || !operation.accepting || operation.cancel.signal.aborted) throw new MessageRejected("OpenCode input is closed")
        const admission = Effect.runPromise(this.sdk.sessions.prompt({ sessionID: this.nativeSessionId, text: steeringText(message.content), delivery: "steer" }))
        operation.admission = admission
        await admission
      })
      await Effect.runPromise(this.execution.awaitIdle(this.nativeSessionId))
      operation.accepting = false
      await operation.admission
      await Effect.runPromise(this.execution.awaitIdle(this.nativeSessionId))
      await Promise.all(operation.jobs)
      if (operation.failure !== undefined) throw operation.failure
      turn.signal.throwIfAborted()
      const messages = await Effect.runPromise(this.store.messages({ sessionID: this.nativeSessionId, order: "desc" }))
      const terminal = messages.find(message => message.type === "idle")
      if (!terminal || terminal.type !== "idle" || terminal.outcome !== "succeeded") throw new Error("OpenCode execution did not succeed")
      const inputIndex = messages.findIndex(message => message.id === admitted.id)
      if (inputIndex < 0) throw new Error("OpenCode admitted input is missing from native history")
      const message = messages.slice(0, inputIndex).find(message => message.type === "assistant")
      const response = message?.type === "assistant" ? message.content.filter(part => part.type === "text").map(part => part.text).join("\n") : undefined
      return { nativeSessionId: this.nativeSessionId, response }
    } catch (error) {
      await this.stop(operation)
      throw error
    } finally {
      operation.cancel.abort()
      await Promise.allSettled(operation.jobs)
      turn.signal.removeEventListener("abort", cancelled)
      this.active = undefined
    }
  }

  private stop(operation: Operation): Promise<void> {
    return operation.stop ??= (async () => {
      operation.accepting = false
      operation.cancel.abort()
      try {
        await operation.admission.catch(() => {})
        for (const session of await Effect.runPromise(this.store.list())) {
          await Effect.runPromise(this.execution.interrupt(session.id, { resume: false, awaitSettlement: true }))
        }
        await Promise.allSettled(operation.jobs)
        // Background notifications run in the host scope and can otherwise wake
        // an already-interrupted parent. Closing the scope joins those fibers.
        // The next Turn reconstructs and retires their saved pending input.
        await this.closeHost()
      } catch (error) { this.closed = true; throw error }
    })()
  }

  private async closeHost() {
    if (this.unsubscribe) { await Effect.runPromise(this.unsubscribe); this.unsubscribe = undefined }
    if (this.scope) await Effect.runPromise(Scope.close(this.scope, Exit.succeed(undefined)))
    this.hostOpen = false
  }

  async close() {
    this.closed = true
    if (this.active) await this.stop(this.active)
    await this.closeHost()
  }
}
