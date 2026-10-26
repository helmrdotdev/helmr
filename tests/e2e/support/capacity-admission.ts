import { setTimeout as delay } from "node:timers/promises"
import { assert, deadline, errorCode, type StartAgent } from "./context"

// Serial policy cases can release a Computer before its physical VM is fenced.
// Admission rolls back on this capacity error, so retry the same native request.
export function capacityAwareStartAgent(startAgent: StartAgent) {
  return async (
    definition: string,
    request: Parameters<StartAgent>[1] & { idempotencyKey: string },
    options: { signal?: AbortSignal } = {},
  ) => {
    assert(request.idempotencyKey.length > 0, "Capacity retries require an explicit idempotency key")
    // Admission has its own bound; subsequent Turn waits keep their existing budgets.
    const admissionSignal = deadline(120_000)
    const signal = options.signal ? AbortSignal.any([admissionSignal, options.signal]) : admissionSignal
    for (;;) {
      signal.throwIfAborted()
      try {
        return await startAgent(definition, request, { signal })
      } catch (error) {
        signal.throwIfAborted()
        if (errorCode(error) !== "execution_capacity_unavailable") throw error
      }
      await delay(500, undefined, { signal })
    }
  }
}
