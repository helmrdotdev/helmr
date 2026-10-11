import { durationMilliseconds } from "../duration"
import type { TurnOutcome, TurnState, TimedTurnWaitResult, TurnWaitOptions } from "../contract"
import { abortableDelay } from "./abort"

export type TurnObservation = Pick<TurnState, "id" | "sessionId" | "status" | "result" | "response" | "error" | "payloadExpiredAt"> & { readonly waitBlocked?: "capacity_wait_blocked" }

// Observation uses the ordinary authenticated read and never sends a control.
export function waitForTurn(sessionId: string, turnId: string, get: (signal: AbortSignal) => Promise<TurnObservation>, options: TurnWaitOptions & { readonly timeout: string | number }): Promise<TimedTurnWaitResult>
export function waitForTurn(sessionId: string, turnId: string, get: (signal: AbortSignal) => Promise<TurnObservation>, options?: TurnWaitOptions & { readonly timeout?: undefined }): Promise<TurnOutcome>
export function waitForTurn(sessionId: string, turnId: string, get: (signal: AbortSignal) => Promise<TurnObservation>, options: TurnWaitOptions): Promise<TurnOutcome | TimedTurnWaitResult>
export async function waitForTurn(
  sessionId: string,
  turnId: string,
  get: (signal: AbortSignal) => Promise<TurnObservation>,
  options: TurnWaitOptions = {},
): Promise<TurnOutcome | TimedTurnWaitResult> {
  const timeout = options.timeout === undefined ? undefined : durationMilliseconds(options.timeout, "Turn timeout")
  const deadline = timeout === undefined ? undefined : performance.now() + timeout
  const timerController = new AbortController()
  const timeoutReason = new Error("Turn observation timed out")
  const signal = options.signal === undefined ? timerController.signal : AbortSignal.any([options.signal, timerController.signal])
  let timer: ReturnType<typeof setTimeout> | undefined
  const expire = () => {
    if (deadline === undefined) return
    const remaining = deadline - performance.now()
    if (remaining <= 0) timerController.abort(timeoutReason)
    else timer = setTimeout(expire, Math.min(remaining, 2147483647))
  }
  expire()
  try {
    for (;;) {
      if (deadline !== undefined && performance.now() >= deadline) timerController.abort(timeoutReason)
      signal.throwIfAborted()
      const state = await readWithAbort(get, signal)
      if (deadline !== undefined && performance.now() >= deadline) timerController.abort(timeoutReason)
      signal.throwIfAborted()
      if (state.id !== turnId || state.sessionId !== sessionId) throw new Error("Turn response changed identity")
      if (state.status !== "queued" && state.status !== "running" && state.status !== "finalizing") {
        const outcome: TurnOutcome = Object.freeze({
          status: state.status,
          ...(state.result === undefined ? {} : { result: state.result }),
          ...(state.response === undefined ? {} : { response: state.response }),
          ...(state.error === undefined ? {} : { error: state.error }),
          ...(state.payloadExpiredAt === undefined ? {} : { payloadExpiredAt: state.payloadExpiredAt }),
        })
        return timeout === undefined ? outcome : Object.freeze({ status: "settled", outcome })
      }
      if (state.waitBlocked === "capacity_wait_blocked") throw Object.assign(new Error("Waiting depends on releasing the caller's retained Computer allocation"), { code: "capacity_wait_blocked" })
      await abortableDelay(500, signal)
    }
  } catch (error) {
    if (error === timeoutReason) return Object.freeze({ status: "timeout" })
    throw error
  } finally {
    if (timer !== undefined) clearTimeout(timer)
  }
}

async function readWithAbort(get: (signal: AbortSignal) => Promise<TurnObservation>, signal: AbortSignal): Promise<TurnObservation> {
  signal.throwIfAborted()
  let onAbort!: () => void
  const aborted = new Promise<never>((_, reject) => {
    onAbort = () => reject(signal.reason)
    signal.addEventListener("abort", onAbort, { once: true })
  })
  try {
    return await Promise.race([
      Promise.resolve().then(() => {
        signal.throwIfAborted()
        return get(signal)
      }),
      aborted,
    ])
  } finally {
    signal.removeEventListener("abort", onAbort)
  }
}
