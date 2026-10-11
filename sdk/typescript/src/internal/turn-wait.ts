import type { TurnOutcome, TurnState, TimedTurnWaitResult, TurnWaitOptions } from "../contract"
import { abortableDelay } from "./abort"

export type TurnObservation = Pick<TurnState, "id" | "sessionId" | "status" | "result" | "response" | "error" | "payloadExpiredAt"> & { readonly waitBlocked?: "capacity_wait_blocked" }

// Observation uses the ordinary authenticated read and never sends a control.
export function waitForTurn(sessionId: string, turnId: string, retrieve: (signal: AbortSignal) => Promise<TurnObservation>, options: TurnWaitOptions & { readonly timeout: string | number }): Promise<TimedTurnWaitResult>
export function waitForTurn(sessionId: string, turnId: string, retrieve: (signal: AbortSignal) => Promise<TurnObservation>, options?: TurnWaitOptions & { readonly timeout?: undefined }): Promise<TurnOutcome>
export function waitForTurn(sessionId: string, turnId: string, retrieve: (signal: AbortSignal) => Promise<TurnObservation>, options: TurnWaitOptions): Promise<TurnOutcome | TimedTurnWaitResult>
export async function waitForTurn(
  sessionId: string,
  turnId: string,
  retrieve: (signal: AbortSignal) => Promise<TurnObservation>,
  options: TurnWaitOptions = {},
): Promise<TurnOutcome | TimedTurnWaitResult> {
  const timeout = options.timeout === undefined ? undefined : timeoutMilliseconds(options.timeout)
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
      const state = await readWithAbort(retrieve, signal)
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

async function readWithAbort(retrieve: (signal: AbortSignal) => Promise<TurnObservation>, signal: AbortSignal): Promise<TurnObservation> {
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
        return retrieve(signal)
      }),
      aborted,
    ])
  } finally {
    signal.removeEventListener("abort", onAbort)
  }
}

function timeoutMilliseconds(value: string | number): number {
  let milliseconds: number
  if (typeof value === "number") milliseconds = value
  else {
    const match = /^([1-9][0-9]*)(ms|s|m|h|d)$/.exec(value)
    if (!match) throw new Error("Turn timeout must be a positive duration")
    milliseconds = Number(match[1]) * ({ ms: 1, s: 1000, m: 60000, h: 3600000, d: 86400000 }[match[2]!]!)
  }
  if (!Number.isSafeInteger(milliseconds) || milliseconds <= 0) throw new Error("Turn timeout must be positive safe integer milliseconds")
  return milliseconds
}
