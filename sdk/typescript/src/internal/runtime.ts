import type {
  Duration,
  JsonValue,
  Metadata,
  SendOptions,
} from "../contract"
import type { LogAttributes, RunLogLevel } from "../logger"
import type {
  TokenCreateOptions,
  TokenCreateResult,
  TokenWaitOptions,
} from "../tokens"

const runtimeOperationsSymbol = Symbol.for("helmr.sdk.v0.runtime_operations")

export interface RuntimeOperations {
  readonly waitFor: (duration: Duration) => Promise<void>
  readonly waitUntil: (date: Date) => Promise<void>
  readonly actorInputSend: (
    target: Readonly<{
      declaredId: string
      address: { readonly id: string } | { readonly key: string }
    }>,
    input: JsonValue,
    options?: SendOptions,
  ) => Promise<{ sequence: number }>
  readonly tokenCreate: (
    options: TokenCreateOptions,
  ) => Promise<Omit<TokenCreateResult, "wait">>
  readonly tokenWait: (
    tokenId: string,
    options: TokenWaitOptions,
  ) => Promise<JsonValue>
  readonly metadataSet: (key: string, value: JsonValue) => Promise<void>
  readonly metadataPatch: (values: Metadata) => Promise<void>
  readonly metadataIncrement: (key: string, amount: number) => Promise<void>
  readonly structuredLog: (
    level: RunLogLevel,
    message: string,
    attributes: LogAttributes,
  ) => Promise<void>
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

export function runtimeOperationsInstalled(): boolean {
  return (globalThis as RuntimeGlobal)[runtimeOperationsSymbol] !== undefined
}
