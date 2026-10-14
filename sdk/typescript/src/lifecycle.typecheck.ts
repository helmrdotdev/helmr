import type { CommandCancelReceipt, CommandRef, CommandInfo, Computer, Duration, Retention, Message, TurnOutcome, SessionEvent, SessionRef, ComputerRef } from "./index"
import { isHelmrError, ContentError } from "./index"

export function lifecycleTypes(outcome: TurnOutcome<{ count: number }>, command: CommandInfo, computer: Computer, event: SessionEvent, message: Message, error: unknown, session: SessionRef, ref: ComputerRef) {
  if (outcome.status === "completed" && outcome.payloadExpiredAt === undefined) {
    const count: number = outcome.result.count
    void count
  }
  if (outcome.status === "failed") {
    // @ts-expect-error failed Turns cannot publish a result
    outcome.result.count
  }
  if (outcome.payloadExpiredAt !== undefined) {
    // @ts-expect-error expired payload cannot have a result
    outcome.result.count
  }
  if (command.status === "failed" || command.status === "lost") {
    const kind: "system_failed" = command.outcome.kind
    void kind
  }
  if (command.status === "cancelled") { const kind: "cancelled" = command.outcome.kind; void kind }
  if (computer.residency === "unavailable") { const reason: string = computer.error.code; void reason }
  if (event.kind === "turn.output") { const text = event.data[0]; void text }
  if (event.kind === "message.rejected") { const delivery: "uncertain" | "not_delivered" = event.data.delivery; void delivery }
  if (message.source.kind === "session") { const id: string = message.source.requesterSessionId; void id }
  if (isHelmrError(error)) { const code: string = error.code; void code }
  if (error instanceof ContentError) { const code: ContentError["code"] = error.code; void code }
  void session.interrupt(); void session.turns.list(); void ref.update({ deleteAfterUnused: "never" })
  void ref.exec({ command: ["true"], idempotencyKey: "operation" })
}
const retention: Retention = "never"
// @ts-expect-error execution durations cannot opt out with retention's never
const duration: Duration = "never"
// @ts-expect-error arbitrary strings do not describe a duration
const invalid: Duration = "soon"
// @ts-expect-error a schedule starts a Turn; it cannot send a live message
const source: Message["source"] = { kind: "schedule" }
void retention; void duration; void invalid; void source

export function commandCancellation(command: CommandRef): Promise<CommandCancelReceipt> {
  return command.cancel()
}
