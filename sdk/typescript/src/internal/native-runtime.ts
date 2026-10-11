import type { ChildProcessWithoutNullStreams, SpawnOptions } from "node:child_process"
import type { Turn } from "../agent"

// Private contract between qualified adapters and their owning runtime. Bundle
// copies share the installed hook; object identity still binds each actual Turn.
const nativeRuntimeSymbol = Symbol.for("helmr.agent.v1.native_runtime")

export interface NativeProcess {
  readonly child: ChildProcessWithoutNullStreams
  readonly ready: Promise<{ readonly scopeId: string; readonly processId: number }>
}
export interface NativeOperation {
  // Lifetime completion, independent of the caller-facing operation result.
  // A caught native result error can be retried after qualified idle convergence.
  readonly done: Promise<void>
  stop(reason: unknown): Promise<void>
  state(): { readonly idle: boolean; readonly reusable: boolean }
}
export interface NativeResourceLifecycle {
  readonly initialized: Promise<void>
  state(): { readonly phase: "initializing" | "idle" | "active" | "closed"; readonly reusable: boolean }
  stop(reason: unknown): Promise<void>
}
export interface NativeResource { readonly signal: AbortSignal; readonly cooperativeStopMs: number }
export interface NativeInvocation {
  run<T>(callback: () => T): T
}
export interface NativeRuntimeHooks {
  resource(lifecycle: NativeResourceLifecycle): NativeResource
  spawn(resource: NativeResource, command: string, args: readonly string[], options: Pick<SpawnOptions, "cwd" | "env" | "signal">): NativeProcess
  register(turn: Turn, resource: NativeResource, operation: NativeOperation): NativeInvocation
}

type NativeGlobal = typeof globalThis & { [nativeRuntimeSymbol]?: NativeRuntimeHooks }

export function installNativeRuntime(hooks: NativeRuntimeHooks): () => void {
  const target = globalThis as NativeGlobal
  if (target[nativeRuntimeSymbol] !== undefined) throw new Error("Native runtime is already installed")
  const installed = Object.freeze(hooks)
  target[nativeRuntimeSymbol] = installed
  return () => { if (target[nativeRuntimeSymbol] === installed) delete target[nativeRuntimeSymbol] }
}

function nativeRuntime(): NativeRuntimeHooks {
  const hooks = (globalThis as NativeGlobal)[nativeRuntimeSymbol]
  if (!hooks) throw new Error("Native operation requires the managed Session runtime")
  return hooks
}

export function registerNativeResource(lifecycle: NativeResourceLifecycle): NativeResource {
  return nativeRuntime().resource(lifecycle)
}
export function spawnNative(resource: NativeResource, command: string, args: readonly string[], options: Pick<SpawnOptions, "cwd" | "env" | "signal">): NativeProcess {
  return nativeRuntime().spawn(resource, command, args, options)
}
export function registerNativeOperation(turn: Turn, resource: NativeResource, operation: NativeOperation): NativeInvocation {
  return nativeRuntime().register(turn, resource, operation)
}
