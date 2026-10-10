import type { InputContent } from "../../../sdk/typescript/src/content"
import { normalizeQuestion, type Question, type AnswerControl, type AnswerFor, type AskResponse } from "../../../sdk/typescript/src/question"
import { AgentOutputWriter } from "./agent-output"
import { OperationError } from "./agent-channel"
import { ContentError, normalizeContent, normalizeInput, type Content, type HumanContent } from "../../../sdk/typescript/src/content"
import type { SessionActivity } from "./agent-activity"
import { randomUUID, randomUUIDv7 } from "node:crypto"
import { AsyncLocalStorage } from "node:async_hooks"
import type { AgentDefinition, AgentContext, Json, OutputReceipt, SetupContext, Turn, TurnOutcome } from "../../../sdk/typescript/src/agent"
import { NativeContinuationLost, type SessionNativeRegistry } from "./agent-native"
import { MessageRejected } from "../../../sdk/typescript/src/message-error"
import { requireJson } from "../../../sdk/typescript/src/agent"

export interface AdmittedTurn {
  readonly id: string
  readonly sequence: number
  readonly createdAt: string
  readonly input: InputContent
  readonly source: Turn["source"]
}
export interface AskRegistration {
  readonly answer: Promise<AskResponse>
  // Idempotent, including when an answer wins concurrently with withdrawal.
  withdraw(): Promise<void>
}
export interface TurnTiming {
  readonly sessionId: string
  readonly turnId: string
  readonly sequence: number
  readonly outcome: TurnOutcome["status"]
  readonly handlerReturnToSettlementMs: number
  readonly handlerReturnToFinalizationMs: number
  readonly finalizationToSettlementMs: number
}
// All operations are bound to this Session's current authority by the guest
// transport. Reconnection must renew ownership without reconstructing this object.
export interface SessionDriver {
  registerMessages(turnId: string): Promise<void>
  // Idempotent; drainage can retry after an uncertain boundary response.
  closeProcessing(turnId: string): Promise<void>
  ask(turnId: string, creationId: string, question: Question, signal: AbortSignal): Promise<AskRegistration>
  output(turnId: string, outputId: string, value: Content): Promise<OutputReceipt>
  respond(turnId: string, responseId: string, value: Content): Promise<void>
  // An aborted signal requests stopping; it must not skip join/fencing evidence.
  // Idempotent; failure calls first stop native work and then verify again after
  // authored work has joined. Calls may overlap when interruption supersedes success.
  convergeNative(turnId: string, outcome: "returned" | "failed", signal: AbortSignal): Promise<void>
  finalize(turnId: string, result: Json): Promise<TurnOutcome>
  fail(turnId: string, error: { code: string; message: string }): Promise<TurnOutcome>
  hold(reason: string): Promise<void>
}
interface AskOperation {
  registration: Promise<AskRegistration>
  complete: boolean
  withdrawal?: Promise<void>
}
interface Execution {
  request: AdmittedTurn
  fingerprint: string
  abort: AbortController
  signal: AbortSignal
  phase: "processing" | "draining" | "finalizing" | "settling_failure" | "terminal"
  promise: Promise<TurnOutcome>
  handler?: Promise<Json>
  output: Set<Promise<unknown>>
  pipes: Set<Promise<unknown>>
  registrations: Set<Promise<unknown>>
  asks: Set<AskOperation>
  messageHandler?: (message: InputContent) => unknown
  callback?: Promise<void> | undefined
  messages: Map<string, { fingerprint: string; promise: Promise<void> }>
  result?: Json
  failure?: { code: string; message: string }
  settlement?: Promise<TurnOutcome> | undefined
  outcome?: TurnOutcome
  handlerReturnedAt?: number
  finalizationStartedAt?: number
}
interface OutputScope { execution: Execution; live: boolean }

function fingerprint(value: unknown): string {
  return JSON.stringify(value, (_key, item) => item !== null && typeof item === "object" && !Array.isArray(item)
    ? Object.fromEntries(Object.entries(item).sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0)) : item)
}

function aborted<T>(pending: Promise<T>, signal: AbortSignal): Promise<T> {
  return new Promise((resolve, reject) => {
    const stop = () => reject(signal.reason)
    signal.addEventListener("abort", stop, { once: true })
    // Always observe the pending operation, even for an already aborted signal.
    pending.then(resolve, reject).finally(() => signal.removeEventListener("abort", stop))
    if (signal.aborted) stop()
  })
}

async function bounded<T>(pending: Promise<T>, milliseconds: number): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined
  try {
    return await Promise.race([pending, new Promise<never>((_resolve, reject) => {
      timer = setTimeout(() => reject(new Error("Turn failure convergence timed out")), milliseconds)
    })])
  } finally { clearTimeout(timer) }
}

export class SessionRuntime {
  private setup?: Promise<AgentContext<unknown>>
  private active: Execution | undefined
  private readonly turns = new Map<string, Execution>()
  private retiredThrough = 0
  private dispatchedThrough = 0
  private held = false
  private reconstructRequired = false
  private readonly outputScope = new AsyncLocalStorage<OutputScope>()
  private readonly definition: AgentDefinition<InputContent, Json, unknown>
  private readonly context: SetupContext
  private readonly driver: SessionDriver
  private readonly failureConvergenceMs: number
  private readonly native: SessionNativeRegistry | undefined
  private readonly activity: SessionActivity | undefined
  private readonly onTiming: ((timing: TurnTiming) => void) | undefined
  constructor(definition: AgentDefinition<InputContent, Json, unknown>, context: SetupContext, driver: SessionDriver, limits: { failureConvergenceMs: number; terminalSequence?: number; native?: SessionNativeRegistry; activity?: SessionActivity; onTiming?: (timing: TurnTiming) => void } = { failureConvergenceMs: 10_000 }) {
    this.definition = definition
    this.context = context
    this.driver = driver
    this.native = limits.native
    this.activity = limits.activity
    this.onTiming = limits.onTiming
    if (!Number.isFinite(limits.failureConvergenceMs) || limits.failureConvergenceMs <= 0) throw new Error("Failure convergence bound must be positive")
    this.failureConvergenceMs = limits.failureConvergenceMs
    const terminalSequence = limits.terminalSequence ?? 0
    if (!Number.isSafeInteger(terminalSequence) || terminalSequence < 0) throw new Error("Invalid terminal sequence")
    this.retiredThrough = terminalSequence
    this.dispatchedThrough = terminalSequence
  }

  // Retaining the Promise also retains a failed setup. Only a new explicitly
  // reconstructed process may invoke setup again; retrying delivery cannot.
  initialize(): Promise<AgentContext<unknown>> {
    return this.setup ??= (async () => {
      try {
        const action = () => this.definition.setup?.(this.context)
        const setup = () => this.activity ? this.activity.authored(action) : action()
        return { ...this.context, setupResult: await (this.native ? this.native.setup(setup) : setup()) }
      }
      catch (error) { this.held = true; this.reconstructRequired = true; return this.recordFailureHold("setup_failed", error) }
    })()
  }

  dispatch(request: AdmittedTurn): Promise<TurnOutcome> {
    try { normalizeInput(request.input) } catch (error) { return Promise.reject(error) }
    const identity = fingerprint(request)
    const old = this.turns.get(request.id)
    if (old) {
      if (old.fingerprint !== identity) return Promise.reject(new Error("Turn delivery identity changed"))
      return old.promise
    }
    if (this.held || this.active) return Promise.reject(new Error("Session cannot dispatch another Turn"))
    if (!Number.isSafeInteger(request.sequence) || request.sequence <= Math.max(this.retiredThrough, this.dispatchedThrough)) return Promise.reject(new Error("Turn delivery has already been acknowledged or has an invalid sequence"))
    const abort = new AbortController()
    const execution: Execution = { request: structuredClone(request), fingerprint: identity, abort, signal: AbortSignal.any([abort.signal, this.context.signal]), phase: "processing", promise: undefined!, output: new Set(), pipes: new Set(), registrations: new Set(), asks: new Set(), messages: new Map() }
    this.dispatchedThrough = request.sequence
    this.turns.set(request.id, execution)
    this.active = execution
    execution.promise = this.execute(execution)
    return execution.promise
  }

  // Receipts retain exact identity. A callback rejection is not an enqueue or a
  // reason to run the callback again after transport reconnection.
  deliverMessage(turnId: string, messageId: string, message: InputContent): Promise<void> {
    const execution = this.turns.get(turnId)
    if (!execution) return Promise.reject(new Error("Turn is unknown"))
    try { normalizeInput(message) } catch (error) { return Promise.reject(error) }
    const identity = fingerprint(message)
    const old = execution.messages.get(messageId)
    if (old) return old.fingerprint === identity ? old.promise : Promise.reject(new Error("Message delivery identity changed"))
    if (execution.signal.aborted) return Promise.reject(execution.signal.reason)
    if (execution.phase !== "processing" || !execution.messageHandler) return Promise.reject(new Error("Turn is not accepting a message callback"))
    if (execution.callback) return Promise.reject(new MessageBusy())
    const scope: OutputScope = { execution, live: true }
    const admitted = structuredClone(message)
    const callback = Promise.resolve().then(() => { execution.signal.throwIfAborted(); return this.invoke(() => this.outputScope.run(scope, () => execution.messageHandler!(admitted))) }).then(() => {}).finally(() => { scope.live = false })
    execution.messages.set(messageId, { fingerprint: identity, promise: callback })
    execution.callback = callback
    void callback.finally(() => { if (execution.callback === callback) execution.callback = undefined }).catch(() => {})
    return callback
  }

  reconcileFinalization(turnId: string): Promise<TurnOutcome> {
    const execution = this.turns.get(turnId)
    if (!execution || (execution.phase !== "finalizing" && execution.phase !== "settling_failure")) return Promise.reject(new Error("Turn has no recorded finalization"))
    execution.promise = this.settle(execution)
    return execution.promise
  }

  interrupt(turnId: string, reason: unknown): void {
    const execution = this.turns.get(turnId)
    if (!execution || execution.phase === "terminal") return
    this.held = true
    if (execution.phase === "processing") execution.phase = "draining"
    this.abort(execution.abort, reason)
  }

  get idle(): boolean { return !this.active && !this.held }
  get canCheckpoint(): boolean { return !this.active && !this.reconstructRequired }

  suspend(reason: unknown): void {
    this.held = true
    if (this.active) this.interrupt(this.active.request.id, reason)
  }

  // The command reader stays live while an interrupted Turn drains. Its caller
  // must recheck control ordering before releasing admission after this wait.
  async waitForResume(): Promise<void> {
    if (this.held && this.active) await this.active.promise
  }

  // Guest-only: current control-plane authority must have released all effective
  // holds before calling. A failed setup or unjoined operation cannot be repaired
  // by releasing this local admission gate.
  resume(): void {
    this.context.signal.throwIfAborted()
    if (!this.held) return
    if (this.active || this.reconstructRequired) throw new Error("Session requires reconciliation or process reconstruction")
    this.held = false
  }

  // The bridge calls this only after durable terminal-state reconciliation.
  // Retirement bounds memory while the watermark prevents a delayed delivery
  // from rerunning a handler whose local result has already been released.
  acknowledgeTerminal(turnId: string, sequence: number): void {
    if (sequence <= this.retiredThrough) return
    const execution = this.turns.get(turnId)
    if (!execution || execution.request.sequence !== sequence || execution.phase !== "terminal") throw new Error("Turn is not terminal")
    this.retiredThrough = sequence
    for (const [id, entry] of this.turns) {
      if (entry.request.sequence <= sequence && entry.phase === "terminal") { this.turns.delete(id); this.native?.acknowledge(id) }
    }
  }

  private async execute(execution: Execution): Promise<TurnOutcome> {
    let result: Json
    let context: AgentContext<unknown>
    try { context = await this.initialize() }
    catch (error) {
      // Setup failure leaves the admitted input queued for explicit recovery.
      this.turns.delete(execution.request.id)
      if (this.active === execution) this.active = undefined
      throw error
    }
    try {
      execution.signal.throwIfAborted()
      execution.handler = Promise.resolve().then(() => { execution.signal.throwIfAborted(); return this.invoke(() => this.definition.turn(this.turn(execution), context)) })
      result = await aborted(execution.handler, execution.signal)
      execution.handlerReturnedAt = performance.now()
      requireJson(result)
      result = structuredClone(result)
      execution.phase = "draining"
      await aborted((async () => {
        await Promise.allSettled([...execution.registrations])
        await Promise.allSettled([...execution.asks].map(ask => ask.registration))
        await this.driver.closeProcessing(execution.request.id)
        await Promise.all([...execution.messages.values()].map(message => message.promise.catch(error => { if (!(error instanceof MessageRejected)) throw error })))
        // Producer scopes can admit writes while they drain. Join every producer
        // before taking the final set of durable write acknowledgments.
        await Promise.all([...execution.pipes])
        await Promise.all([...execution.output])
        // An answer resolved in a started callback may finish during drainage.
        await Promise.allSettled([...execution.asks].map(ask => ask.registration))
        await Promise.all([...execution.asks].map(ask => ask.withdrawal))
        if ([...execution.asks].some(ask => !ask.complete)) throw new Error("Turn returned with an unjoined question")
        execution.signal.throwIfAborted()
        await this.driver.convergeNative(execution.request.id, "returned", execution.signal)
      })(), execution.signal)
    } catch (error) {
      let interrupted = execution.signal.aborted || error instanceof NativeContinuationLost
      if (interrupted) this.held = true
      execution.phase = "draining"
      this.abort(execution.abort, error)
      let convergenceExpired = false
      let continuationLost = error instanceof NativeContinuationLost
      const converge = () => this.driver.convergeNative(execution.request.id, "failed", execution.signal).catch(error => {
        if (error instanceof NativeContinuationLost) { continuationLost = true; return }
        throw error
      })
      try {
        const native = converge()
        await bounded(Promise.all([native, (async () => {
          await this.driver.closeProcessing(execution.request.id)
          await Promise.allSettled(execution.handler ? [execution.handler] : [])
          await Promise.allSettled([...execution.registrations])
          await Promise.all([...execution.asks].map(ask => this.withdraw(ask)))
          await Promise.allSettled([...execution.messages.values()].map(message => message.promise))
          await Promise.allSettled([...execution.pipes])
          await Promise.allSettled([...execution.output])
        })()]).then(async () => {
          if (convergenceExpired) return
          await converge()
        }), this.failureConvergenceMs)
      } catch (convergenceError) {
        convergenceExpired = true
        this.held = true
        this.reconstructRequired = true
        return this.recordFailureHold("native_convergence_failed", convergenceError)
      }
      if (continuationLost) {
        interrupted = true
        this.held = true
        this.reconstructRequired = true
        await this.recordHold("native_continuation_lost", error)
      }
      execution.phase = "settling_failure"
      execution.failure = { code: interrupted ? "interrupted" : "handler_failed", message: error instanceof Error ? error.message : String(error) }
      return this.settle(execution)
    }
    execution.phase = "finalizing"
    execution.result = result
    return this.settle(execution)
  }

  private settle(execution: Execution): Promise<TurnOutcome> {
    // Both successful and failed outcomes retain their exact settlement payload
    // after a lost reply. Reconciliation never reruns processing or drainage.
    return execution.settlement ??= Promise.resolve().then(async () => {
      execution.finalizationStartedAt ??= performance.now()
      const outcome = execution.failure
        ? await this.driver.fail(execution.request.id, { ...execution.failure })
        : await this.driver.finalize(execution.request.id, structuredClone(execution.result!))
      const observedAt = performance.now()
      this.terminal(execution, outcome)
      // One process measures the full return/drain/save/retry interval. The end
      // includes delivery of the authoritative outcome, not the remote commit
      // instant. Missing observations after process loss are not reconstructed.
      if (execution.handlerReturnedAt !== undefined) {
        try {
          this.onTiming?.({
            sessionId: this.context.session.id, turnId: execution.request.id,
            sequence: execution.request.sequence, outcome: outcome.status,
            handlerReturnToSettlementMs: observedAt - execution.handlerReturnedAt,
            handlerReturnToFinalizationMs: execution.finalizationStartedAt - execution.handlerReturnedAt,
            finalizationToSettlementMs: observedAt - execution.finalizationStartedAt,
          })
        } catch { /* Diagnostics cannot reverse an authoritative outcome. */ }
      }
      return outcome
    }).finally(() => { execution.settlement = undefined })
  }
  private abort(controller: AbortController, reason: unknown): void {
    const action = () => controller.abort(reason)
    if (this.activity) this.activity.authored(action)
    else action()
  }
  private invoke<T>(action: () => T): T {
    return this.activity ? this.activity.authored(action) : action()
  }
  private async recordFailureHold(reason: string, original: unknown): Promise<never> {
    await this.recordHold(reason, original)
    throw original
  }
  private async recordHold(reason: string, original: unknown): Promise<void> {
    try { await this.driver.hold(reason) }
    catch (secondary) {
      const describe = (error: unknown) => error instanceof Error ? error.message : String(error)
      throw new AggregateError([original, secondary], `${describe(original)}; recording the Session hold failed: ${describe(secondary)}`)
    }
  }
  private withdraw(operation: AskOperation): Promise<void> {
    if (operation.withdrawal) return operation.withdrawal
    if (operation.complete) return Promise.resolve()
    return operation.withdrawal = operation.registration.then(registration => registration.withdraw(), () => {})
  }
  private terminal(execution: Execution, outcome: TurnOutcome): void {
    execution.phase = "terminal"
    execution.outcome = outcome
    if (this.active === execution) this.active = undefined
  }

  private turn(execution: Execution): Turn {
    const open = () => { if (execution.phase !== "processing") throw new Error("Turn processing admission is closed"); execution.signal.throwIfAborted() }
    const outputOpen = () => {
      const scope = this.outputScope.getStore()
      if (execution.phase !== "processing" && !(execution.phase === "draining" && scope?.execution === execution && scope.live)) throw new Error("Turn output is closed")
      execution.signal.throwIfAborted()
    }
    const retainOutput = <T>(pending: Promise<T>): Promise<T> => {
      const tracked = pending.catch(error => { if (!contentAdmissionRejected(error)) throw error })
      execution.output.add(tracked)
      void tracked.catch(() => {})
      void pending.catch(() => {})
      return pending
    }
    const writer = new AgentOutputWriter(content => {
      execution.signal.throwIfAborted()
      return retainOutput(this.driver.output(execution.request.id, randomUUID(), content))
    })
    const output = (value: HumanContent): Promise<OutputReceipt> => { outputOpen(); return retainOutput(writer.write(value)) }
    const track = <T>(pending: Promise<T>): Promise<T> => { execution.registrations.add(pending); void pending.catch(() => {}); return pending }
    const turn: Turn = {
      ...execution.request,
      signal: execution.signal,
      output: { write: output, pipe: (values, options) => {
        open()
        const scope: OutputScope = { execution, live: true }
        const signal = options?.signal ? AbortSignal.any([execution.signal, options.signal]) : execution.signal
        const pending = this.outputScope.run(scope, () => writer.pipe(values, signal, async cleanup => {
          execution.pipes.add(cleanup); void cleanup.catch(() => {})
          await bounded(cleanup, this.failureConvergenceMs)
        }).finally(() => { scope.live = false }))
        const tracked = pending.catch(error => { if (contentAdmissionRejected(error) || (!execution.signal.aborted && options?.signal?.aborted && error === options.signal.reason)) return; throw error })
        execution.pipes.add(tracked)
        // The retained observer carries failure into drainage; also observe the
        // caller-facing Promise so forgotten writes don't crash the process.
        void tracked.catch(() => {})
        void pending.catch(() => {}); return pending
      } },
      respond: value => { open(); const content = normalizeContent(value); const id = randomUUID(); return track(writer.ordered(() => this.driver.respond(execution.request.id, id, content))) },
      onMessage: handler => { open(); if (execution.messageHandler) throw new Error("Message handler already registered"); execution.messageHandler = handler; return track(this.driver.registerMessages(execution.request.id)) },
      ask: <C extends AnswerControl>(question: Question<C>, options?: { signal?: AbortSignal }) => {
        open(); const frozenQuestion = normalizeQuestion(question)
        const signal = options?.signal ? AbortSignal.any([execution.signal, options.signal]) : execution.signal
        signal.throwIfAborted()
        const operation: AskOperation = { registration: writer.ordered(() => this.driver.ask(execution.request.id, randomUUIDv7(), frozenQuestion, signal)), complete: false }
        execution.asks.add(operation)
        const answer = (async () => {
          const abort = () => { void this.withdraw(operation).catch(() => {}) }
          try {
            const registration = await operation.registration
            signal.addEventListener("abort", abort, { once: true })
            if (signal.aborted) abort()
            const value = await aborted(registration.answer, signal)
            signal.throwIfAborted(); requireJson(value)
            return value as AskResponse<AnswerFor<C>>
          } catch (error) {
            if (signal.aborted) await this.withdraw(operation)
            throw error
          } finally { signal.removeEventListener("abort", abort); operation.complete = true }
        })()
        void answer.catch(() => {})
        return answer
      },
    }
    this.native?.bind(turn, {
      id: execution.request.id,
      assertOpen: open,
      producer: done => {
        const scope: OutputScope = { execution, live: true }
        execution.pipes.add(done)
        void done.finally(() => { scope.live = false }).catch(() => {})
        return { run: callback => this.invoke(() => this.outputScope.run(scope, callback)) }
      },
    })
    return turn
  }
}

// This is transport backpressure, not a terminal delivery rejection. The guest
// bridge must retain the exact accepted message and retry after the callback.
export class MessageBusy extends Error {
  constructor() { super("Turn message callback is busy"); this.name = "MessageBusy" }
}

function contentAdmissionRejected(error: unknown): boolean {
  return (error instanceof ContentError || error instanceof OperationError) && ["content_limit_exceeded", "content_kind_unsupported", "invalid_arguments"].includes(error.code)
}
