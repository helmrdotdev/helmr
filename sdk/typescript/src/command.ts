import type { CommandLogQuery, CommandLogRecord, CommandLogStreamQuery } from "./command-logs"
import type { CursorPage } from "./contract"
import type { Duration } from "./contract"
import type { RequestOptions } from "./request"
import { resourceID } from "./internal/id"
import { timestampString } from "./internal/timestamp"
import { abortableDelay } from "./internal/abort"

export interface CommandWaitOptions extends RequestOptions {
  readonly waitTimeout?: Duration
}

export type CommandOutcome = Readonly<{
  commandId: string
  terminalAt: string
  exitCode?: number
}> & (
  | Readonly<{ kind: "exited"; exitCode: number }>
  | Readonly<{ kind: "cancelled" | "timed_out" }>
  | Readonly<{ kind: "system_failed"; failure: Readonly<{ reason: "guest_failure" | "placement_failed" | "scope_termination_failed" }> }>
)

export interface CommandInfo {
  readonly id: string
  readonly computerId: string
  readonly status: "pending" | "starting" | "running" | "stopping" | "exited" | "failed" | "cancelled" | "timed_out" | "lost"
  readonly processReconciled: boolean
  readonly outcome?: CommandOutcome
}

export interface CancelReceipt {
  readonly id: string
  readonly targetId: string
  readonly status: "accepted"
}

export interface CommandRef {
  readonly id: string
  logs(query?: CommandLogQuery, options?: RequestOptions): Promise<CursorPage<CommandLogRecord>>
  streamLogs(query?: CommandLogStreamQuery, options?: RequestOptions): AsyncIterable<CommandLogRecord>
  cancel(options?: RequestOptions): Promise<CancelReceipt>
  retrieve(options?: RequestOptions): Promise<CommandInfo>
  wait(options?: CommandWaitOptions): Promise<CommandOutcome>
}

export function parseCancelReceipt(value: unknown, expectedId: string): CancelReceipt {
  const raw = object(value, "Cancel receipt")
  const id = resourceID(raw["id"], "Cancel receipt.id")
  const targetId = resourceID(raw["target_id"], "Cancel receipt.target_id")
  if (targetId !== expectedId) throw new Error("Cancel receipt changed target ID")
  if (raw["status"] !== "accepted") throw new Error("Cancel receipt.status is invalid")
  return Object.freeze({ id, targetId, status: "accepted" })
}

export function parseCommandReceipt(value: unknown): string {
  return resourceID(object(value, "Command receipt")["command_id"], "Command receipt.command_id")
}

export function parseCommandInfo(value: unknown, expectedId?: string): CommandInfo {
  const raw = object(value, "Command")
  const id = resourceID(raw["id"], "Command.id")
  if (expectedId !== undefined && id !== expectedId) throw new Error("Command response changed ID")
  const computerId = resourceID(raw["computer_id"], "Command.computer_id")
  const status = raw["status"]
  if (typeof status !== "string" || !["pending", "starting", "running", "stopping", "exited", "failed", "cancelled", "timed_out", "lost"].includes(status)) throw new Error("Command.status is invalid")
  if (typeof raw["process_reconciled"] !== "boolean") throw new Error("Command.process_reconciled is invalid")
  const terminal = ["exited", "failed", "cancelled", "timed_out", "lost"].includes(status)
  if (terminal !== (raw["outcome"] !== undefined)) throw new Error("Command outcome does not match status")
  let outcome: CommandOutcome | undefined
  if (terminal) {
    const wire = object(raw["outcome"], "Command.outcome")
    const commandId = resourceID(wire["command_id"], "Command.outcome.command_id")
    if (commandId !== id) throw new Error("Command outcome changed ID")
    const terminalAt = timestampString(wire["terminal_at"], "Command.outcome.terminal_at")
    const kind = wire["kind"]
    if (kind !== (status === "failed" || status === "lost" ? "system_failed" : status)) throw new Error("Command outcome kind does not match status")
    const exitCode = wire["exit_code"]
    if (exitCode !== undefined && (typeof exitCode !== "number" || !Number.isInteger(exitCode) || exitCode < -2147483648 || exitCode > 2147483647)) throw new Error("Command exit code is invalid")
    const base = { commandId, terminalAt, ...(exitCode === undefined ? {} : { exitCode: exitCode as number }) }
    if (kind === "exited") {
      if (exitCode === undefined) throw new Error("Exited command requires an exit code")
      outcome = Object.freeze({ ...base, kind, exitCode: exitCode as number })
    } else if (kind === "cancelled" || kind === "timed_out") {
      outcome = Object.freeze({ ...base, kind })
    } else {
      const reason = object(wire["failure"], "Command failure")["reason"]
      if (reason !== "guest_failure" && reason !== "placement_failed" && reason !== "scope_termination_failed") throw new Error("Command failure reason is invalid")
      outcome = Object.freeze({ ...base, kind: "system_failed", failure: Object.freeze({ reason }) })
    }
  }
  return Object.freeze({ id, computerId, status: status as CommandInfo["status"], processReconciled: raw["process_reconciled"], ...(outcome === undefined ? {} : { outcome }) })
}

export async function waitForCommand(
  id: string,
  retrieve: (signal?: AbortSignal) => Promise<CommandInfo>,
  options: CommandWaitOptions = {},
): Promise<CommandOutcome> {
  const timeout = options.waitTimeout === undefined ? undefined : waitMilliseconds(options.waitTimeout)
  const deadline = timeout === undefined ? undefined : Date.now() + timeout
  const timeoutController = new AbortController()
  const signal = options.signal === undefined ? timeoutController.signal : AbortSignal.any([options.signal, timeoutController.signal])
  let timer: ReturnType<typeof setTimeout> | undefined
  const expire = () => {
    if (deadline === undefined) return
    const remaining = deadline - Date.now()
    if (remaining <= 0) timeoutController.abort(new DOMException("Command observation timed out", "TimeoutError"))
    else timer = setTimeout(expire, Math.min(remaining, 2147483647))
  }
  expire()
  try {
    for (;;) {
      expireIfDue()
      signal.throwIfAborted()
      const info = await retrieve(signal)
      expireIfDue()
      signal.throwIfAborted()
      if (info.id !== id) throw new Error("Command response changed ID")
      if (info.outcome !== undefined) return info.outcome
      await abortableDelay(Math.max(1, Math.min(1000, deadline === undefined ? 1000 : deadline - Date.now())), signal)
    }
  } finally {
    if (timer !== undefined) clearTimeout(timer)
  }
  function expireIfDue() {
    if (deadline !== undefined && Date.now() >= deadline) timeoutController.abort(new DOMException("Command observation timed out", "TimeoutError"))
  }
}

function waitMilliseconds(value: Duration): number {
  const match = /^([1-9][0-9]*)(ms|s|m|h|d)$/.exec(value)
  if (match === null) throw new Error("Command waitTimeout must be a positive duration")
  const unit = { ms: 1, s: 1000, m: 60000, h: 3600000, d: 86400000 }[match[2]!]!
  const result = Number(match[1]) * unit
  if (!Number.isSafeInteger(result) || !Number.isSafeInteger(Date.now() + result)) throw new Error("Command waitTimeout is too large")
  return result
}

function object(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label} must be an object`)
  return value as Record<string, unknown>
}
