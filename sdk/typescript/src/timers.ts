import type { Duration } from "./contract"
import { currentRuntimeOperations } from "./internal/runtime"

export interface Timers {
  waitFor(duration: Duration): Promise<void>
  waitUntil(date: Date): Promise<void>
}

export const timers: Timers = Object.freeze({
  waitFor(duration: Duration): Promise<void> {
    return currentRuntimeOperations().waitFor(duration)
  },
  waitUntil(date: Date): Promise<void> {
    return currentRuntimeOperations().waitUntil(date)
  },
})
