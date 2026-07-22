import type { Duration } from "../contract"

const runtimeOperationsSymbol = Symbol.for("helmr.sdk.v0.runtime_operations")

export interface RuntimeOperations {
  readonly waitFor: (duration: Duration) => Promise<void>
  readonly waitUntil: (date: Date) => Promise<void>
}

type RuntimeGlobal = typeof globalThis & {
  [runtimeOperationsSymbol]?: RuntimeOperations
}

export function installRuntimeOperations(
  operations: RuntimeOperations,
): () => void {
  const target = globalThis as RuntimeGlobal
  if (target[runtimeOperationsSymbol] !== undefined) {
    throw new Error("Helmr runtime operations are already installed")
  }
  const installed = Object.freeze(operations)
  target[runtimeOperationsSymbol] = installed
  return () => {
    if (target[runtimeOperationsSymbol] === installed) {
      delete target[runtimeOperationsSymbol]
    }
  }
}

export function currentRuntimeOperations(): RuntimeOperations {
  const operations = (globalThis as RuntimeGlobal)[runtimeOperationsSymbol]
  if (operations === undefined) {
    throw new Error(
      "runtime operation is unavailable without the Helmr managed runtime",
    )
  }
  return operations
}
