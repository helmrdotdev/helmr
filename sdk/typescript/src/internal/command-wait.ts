import type { CommandInfo, CommandOutcome, CommandWaitOptions, TimedCommandWaitResult } from "../command"
import type { Duration } from "../duration"
import { durationMilliseconds } from "../duration"
import { abortableDelay } from "./abort"

export function waitForCommand(id: string, read: (signal: AbortSignal) => Promise<CommandInfo>, options: CommandWaitOptions & { readonly timeout: Duration }): Promise<TimedCommandWaitResult>
export function waitForCommand(id: string, read: (signal: AbortSignal) => Promise<CommandInfo>, options?: CommandWaitOptions & { readonly timeout?: undefined }): Promise<CommandOutcome>
export function waitForCommand(id: string, read: (signal: AbortSignal) => Promise<CommandInfo>, options: CommandWaitOptions): Promise<CommandOutcome | TimedCommandWaitResult>
export async function waitForCommand(id: string, read: (signal: AbortSignal) => Promise<CommandInfo>, options: CommandWaitOptions = {}): Promise<CommandOutcome | TimedCommandWaitResult> {
  const timeout = options.timeout === undefined ? undefined : durationMilliseconds(options.timeout, "Command observation timeout")
  const deadline = timeout === undefined ? undefined : performance.now() + timeout
  const timerController = new AbortController()
  const timeoutReason = new Error("Command observation timed out")
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
      const info = await readWithAbort(read, signal)
      if (deadline !== undefined && performance.now() >= deadline) timerController.abort(timeoutReason)
      signal.throwIfAborted()
      if (info.id !== id) throw new Error("Command response changed ID")
      if (info.outcome !== undefined) return timeout === undefined ? info.outcome : Object.freeze({ status: "settled", outcome: info.outcome })
      await abortableDelay(500, signal)
    }
  } catch (error) {
    if (error === timeoutReason) return Object.freeze({ status: "timeout" })
    throw error
  } finally { if (timer !== undefined) clearTimeout(timer) }
}

async function readWithAbort(read: (signal: AbortSignal) => Promise<CommandInfo>, signal: AbortSignal): Promise<CommandInfo> {
  signal.throwIfAborted()
  let onAbort!: () => void
  const aborted = new Promise<never>((_, reject) => {
    onAbort = () => reject(signal.reason)
    signal.addEventListener("abort", onAbort, { once: true })
  })
  try {
    return await Promise.race([Promise.resolve().then(() => { signal.throwIfAborted(); return read(signal) }), aborted])
  } finally { signal.removeEventListener("abort", onAbort) }
}
