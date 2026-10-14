import type { CommandLogQuery, CommandLogRecord, CommandLogStreamQuery, CommandLogReadPage } from "./command-logs"
import type { Duration } from "./contract"
import type { RequestOptions } from "./request"
import { resourceID } from "./internal/id"
import { timestampString } from "./internal/timestamp"
export { waitForCommand } from "./internal/command-wait"

export interface CommandWaitOptions extends RequestOptions {
  readonly timeout?: Duration
}

export type TimedCommandWaitResult =
  | Readonly<{ status: "timeout" }>
  | Readonly<{ status: "settled"; outcome: CommandOutcome }>

export type CommandOutcome = Readonly<{
  commandId: string
  terminalAt: string
  exitCode?: number
}> & (
  | Readonly<{ kind: "exited"; exitCode: number }>
  | Readonly<{ kind: "cancelled" }>
  | Readonly<{ kind: "timed_out" }>
  | Readonly<{ kind: "system_failed"; failure: Readonly<{ reason: "guest_failure" | "dispatch_failed" | "scope_termination_failed" }> }>
)

export type CommandInfo = Readonly<{ id: string; computerId: string; processReconciled: boolean }> & (
  | Readonly<{ status: "pending" | "starting" | "running" | "stopping"; outcome?: never }>
  | Readonly<{ status: "exited"; outcome: Extract<CommandOutcome, { kind: "exited" }> }>
  | Readonly<{ status: "cancelled"; outcome: Extract<CommandOutcome, { kind: "cancelled" }> }>
  | Readonly<{ status: "timed_out"; outcome: Extract<CommandOutcome, { kind: "timed_out" }> }>
  | Readonly<{ status: "failed" | "lost"; outcome: Extract<CommandOutcome, { kind: "system_failed" }> }>
)

export interface CancelReceipt {
  readonly id: string
  readonly targetId: string
  readonly status: "accepted"
}

export interface CommandRef {
  readonly id: string
  readonly logs: Readonly<{
    list(query?: CommandLogQuery, options?: RequestOptions): Promise<CommandLogReadPage>
    stream(query?: CommandLogStreamQuery, options?: RequestOptions): AsyncIterable<CommandLogRecord>
  }>
  cancel(options?: RequestOptions): Promise<CancelReceipt>
  get(options?: RequestOptions): Promise<CommandInfo>
  wait(options: CommandWaitOptions & { readonly timeout: Duration }): Promise<TimedCommandWaitResult>
  wait(options?: CommandWaitOptions & { readonly timeout?: undefined }): Promise<CommandOutcome>
  wait(options: CommandWaitOptions): Promise<CommandOutcome | TimedCommandWaitResult>
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
      if (reason !== "guest_failure" && reason !== "dispatch_failed" && reason !== "scope_termination_failed") throw new Error("Command failure reason is invalid")
      outcome = Object.freeze({ ...base, kind: "system_failed", failure: Object.freeze({ reason }) })
    }
  }
  return Object.freeze({ id, computerId, status: status as CommandInfo["status"], processReconciled: raw["process_reconciled"], ...(outcome === undefined ? {} : { outcome }) } as CommandInfo)
}


function object(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label} must be an object`)
  return value as Record<string, unknown>
}
