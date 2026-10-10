import type { SessionActivity } from "./agent-activity"
import { AsyncLocalStorage } from "node:async_hooks"
import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process"
import type { Readable } from "node:stream"
import type { Turn } from "../../../sdk/typescript/src/agent"
import { installNativeRuntime, type NativeInvocation, type NativeOperation, type NativeProcess, type NativeResource, type NativeResourceLifecycle, type NativeRuntimeHooks } from "../../../sdk/typescript/src/internal/native-runtime"

export class NativeContinuationLost extends Error {
  constructor() { super("Native continuation was lost after process convergence"); this.name = "NativeContinuationLost" }
}

export interface NativeScopeEvidence { readonly scopeId: string; readonly state: "idle" | "stopped" }
export interface NativeTurnBinding {
  readonly id: string
  assertOpen(): void
  producer(done: Promise<void>): NativeInvocation
}
interface Resource {
  readonly token: NativeResource
  readonly abort: AbortController
  readonly lifecycle: NativeResourceLifecycle
  readonly processes: Set<{ native: NativeProcess; exited: boolean }>
}
interface RegisteredOperation { readonly resource: Resource; readonly operation: NativeOperation }

// One registry belongs to one Session process. It retains setup resources and
// exact Turn object bindings across transport reconnection and RAM continuation.
export class SessionNativeRegistry implements NativeRuntimeHooks {
  private readonly setupScope = new AsyncLocalStorage<boolean>()
  private setupOpen = true
  private readonly resources = new Map<NativeResource, Resource>()
  private readonly turns = new WeakMap<Turn, NativeTurnBinding>()
  private readonly operations = new Map<string, Set<RegisteredOperation>>()
  private readonly signal: AbortSignal
  private readonly cooperativeStopMs: number
  private readonly activity: SessionActivity | undefined
  private readonly spawnProcess: typeof spawn
  constructor(signal: AbortSignal, failureConvergenceMs = 10_000, spawnProcess: typeof spawn = spawn, activity?: SessionActivity) {
    if (!Number.isFinite(failureConvergenceMs) || failureConvergenceMs <= 0) throw new Error("Native convergence bound must be positive")
    this.signal = signal
    this.activity = activity
    this.spawnProcess = spawnProcess
    // Leave half the convergence budget for destructive escalation, guest proof
    // and already-admitted callback drainage if cooperative interruption fails.
    this.cooperativeStopMs = Math.max(1, Math.floor(failureConvergenceMs / 2))
  }

  install(): () => void {
    return installNativeRuntime({ resource: lifecycle => this.resource(lifecycle), spawn: (resource, command, args, options) => this.spawn(resource, command, args, options), register: (turn, resource, operation) => this.register(turn, resource, operation) })
  }

  async setup<T>(action: () => T | Promise<T>): Promise<T> {
    try {
      const result = await this.setupScope.run(true, action)
      this.setupOpen = false
      // Registration precedes the adapter's first await, including forgotten
      // opens. Ready means protocol initialization as well as process startup.
      await Promise.all([...this.resources.values()].map(resource => resource.lifecycle.initialized))
      return result
    } finally { this.setupOpen = false }
  }

  resource(lifecycle: NativeResourceLifecycle): NativeResource {
    if (!this.setupScope.getStore() || !this.setupOpen) throw new Error("Native resources must be opened during Session setup")
    this.signal.throwIfAborted()
    const abort = new AbortController()
    const token = Object.freeze({ signal: AbortSignal.any([this.signal, abort.signal]), cooperativeStopMs: this.cooperativeStopMs })
    this.resources.set(token, { token, abort, lifecycle, processes: new Set() })
    void lifecycle.initialized.catch(() => {})
    return token
  }

  spawn(token: NativeResource, command: string, args: readonly string[], options: Parameters<NativeRuntimeHooks['spawn']>[3]): NativeProcess {
    const resource = this.resources.get(token)
    if (!resource) throw new Error("Native resource does not belong to this Session")
    token.signal.throwIfAborted()
    if (resource.lifecycle.state().phase !== "initializing") throw new Error("Native process admission is closed")
    const signal = options.signal ? AbortSignal.any([token.signal, options.signal]) : token.signal
    const launch = () => this.spawnProcess("/opt/helmr/native/launch", ["__helmr-native-proxy", command, ...args], { ...options, signal, stdio: ["pipe", "pipe", "pipe", "pipe"] })
    const child = this.activity ? this.activity.native(launch) : launch()
    const metadata = child.stdio[3] as Readable
    const ready = new Promise<{ scopeId: string; processId: number }>((resolve, reject) => {
      let body = ""
      metadata.setEncoding("utf8")
      metadata.on("data", (chunk: string) => {
        body += chunk
        if (Buffer.byteLength(body) > 1024) { reject(new Error("Native identity exceeds its bound")); child.kill("SIGKILL") }
      })
      metadata.once("error", reject)
      child.once("error", reject)
      metadata.once("end", () => {
        try {
          const value: unknown = JSON.parse(body)
          if (value === null || typeof value !== "object" || !("scopeId" in value) || typeof value.scopeId !== "string" || !value.scopeId || !("processId" in value) || typeof value.processId !== "number" || !Number.isSafeInteger(value.processId) || value.processId <= 0) throw new Error("Invalid native identity")
          // processId is guest diagnostic metadata, never a signal target.
          resolve({ scopeId: value.scopeId, processId: value.processId })
        } catch (error) { reject(error) }
      })
    })
    void ready.catch(() => {})
    const native: NativeProcess = { child: child as ChildProcessWithoutNullStreams, ready }
    const process = { native, exited: false }
    resource.processes.add(process)
    child.once("close", () => { process.exited = true })
    return native
  }

  bind(turn: Turn, binding: NativeTurnBinding): void {
    if (this.turns.has(turn)) throw new Error("Native Turn already bound")
    this.turns.set(turn, binding)
  }

  register(turn: Turn, token: NativeResource, operation: NativeOperation): NativeInvocation {
    const binding = this.turns.get(turn)
    const resource = this.resources.get(token)
    if (!binding || !resource) throw new Error("Native operation requires its exact Session and Turn")
    binding.assertOpen()
    token.signal.throwIfAborted()
    let operations = this.operations.get(binding.id)
    if (!operations) { operations = new Set(); this.operations.set(binding.id, operations) }
    operations.add({ resource, operation })
    void operation.done.catch(() => {})
    return binding.producer(operation.done)
  }

  async join(turnId: string, disposition: "returned" | "failed", reason?: unknown): Promise<void> {
    const operations = [...(this.operations.get(turnId) ?? [])]
    if (disposition === "failed") {
      await Promise.all(operations.map(async ({ operation }) => {
        if (!operation.state().idle) await operation.stop(reason)
        await operation.done.catch(() => {})
      }))
    } else { await Promise.all(operations.map(({ operation }) => operation.done)) }
  }

  async evidence(): Promise<{ scopes: NativeScopeEvidence[]; reusable: boolean }> {
    const scopes: NativeScopeEvidence[] = []
    let reusable = true
    for (const resource of this.resources.values()) {
      await resource.lifecycle.initialized
      const state = resource.lifecycle.state()
      if (state.phase !== "idle" && state.phase !== "closed") throw new Error("Native resource has unfinished work")
      reusable &&= state.reusable && state.phase === "idle"
      for (const process of resource.processes) {
        const { scopeId } = await process.native.ready
        if (state.phase === "closed" || process.exited) {
          reusable = false
          if (!process.exited) throw new Error("Closed native process has not exited")
          scopes.push({ scopeId, state: "stopped" })
        } else {
          scopes.push({ scopeId, state: "idle" })
        }
      }
    }
    return { scopes, reusable }
  }

  acknowledge(turnId: string): void { this.operations.delete(turnId) }

  async stop(reason: unknown): Promise<void> {
    for (const resource of this.resources.values()) resource.abort.abort(reason)
    await Promise.all([...this.resources.values()].map(resource => resource.lifecycle.stop(reason)))
  }
}
