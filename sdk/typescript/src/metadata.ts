import type { JsonValue, Metadata } from "./contract"
import { currentRuntimeOperations } from "./internal/runtime"

export const metadata = Object.freeze({
  set(key: string, value: JsonValue): Promise<void> {
    return currentRuntimeOperations().metadataSet(key, value)
  },
  patch(values: Metadata): Promise<void> {
    return currentRuntimeOperations().metadataPatch(values)
  },
  increment(key: string, amount = 1): Promise<void> {
    return currentRuntimeOperations().metadataIncrement(key, amount)
  },
})
