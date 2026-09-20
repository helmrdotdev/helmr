import type {
  JsonValue,
  Session,
  SessionDispatch,
  SessionFailure,
  SessionStatus,
  TurnSource,
  TurnState,
  SessionAdmissionReceipt,
  SessionMessageReceipt,
  SessionCloseReceipt,
  SessionCancelReceipt,
  TurnInterruptReceipt,
  SessionResumeReceipt,
  SessionRecoveryReceipt,
  SessionEvent,
  SessionEventKind,
  SessionEventPage,
  OutputReceipt,
} from "../contract"
import { validateTaskId } from "../schema/task"
import { canonicalizeJsonValue } from "./jsoncanon"
import { resourceID } from "./id"
import { timestampString } from "./timestamp"

export function parseSession(value: unknown): Session {
  const v = objectValue(value, "Session")
  const status = sessionStatus(v["status"])
  const actorId = requiredString(v["actor_id"], "actor_id")
  validateTaskId(actorId)
  const failure =
    v["failure"] === undefined ? undefined : parseSessionFailure(v["failure"])
  if ((status === "failed") !== (failure !== undefined))
    throw new Error("Session response has an inconsistent failure projection")
  return Object.freeze({
    id: resourceID(v["id"], "Session.id"),
    actorId,
    deploymentId: resourceID(v["deployment_id"], "Session.deployment_id"),
    workspaceId: resourceID(v["workspace_id"], "Session.workspace_id"),
    ...(v["key"] === undefined
      ? {}
      : { key: requiredString(v["key"], "Session.key") }),
    status,
    ...(v["cancel_requested_at"] === undefined ? {} : {cancelRequestedAt: timestampString(v["cancel_requested_at"], "Session.cancel_requested_at")}),
    createdAt: timestampString(v["created_at"], "Session.created_at"),
    updatedAt: timestampString(v["updated_at"], "Session.updated_at"),
    currentRunId: nullableID(v["current_run_id"], "Session.current_run_id"),
    activeTurnId: nullableID(v["active_turn_id"], "Session.active_turn_id"),
    dispatch: parseDispatch(v["dispatch"]),
    ...(failure === undefined ? {} : { failure }),
  })
}
function parseDispatch(value: unknown): SessionDispatch {
  const v = objectValue(value, "Session.dispatch")
  if (v["state"] === "ready") return Object.freeze({ state: "ready" })
  if (
    v["state"] !== "held" ||
    ![
      "interrupt_requested",
      "interrupted",
      "recovery_required",
      "recovered",
    ].includes(v["reason"] as string)
  )
    throw new Error("Session.dispatch is invalid")
  return Object.freeze({
    state: "held",
    holdId: resourceID(v["hold_id"], "Session.dispatch.hold_id"),
    reason: v["reason"] as Extract<
      SessionDispatch,
      { state: "held" }
    >["reason"],
  })
}
export function parseTurnSource(value: unknown): TurnSource {
  const v = objectValue(value, "Turn.source")
  if (v["type"] === "external") return Object.freeze({ type: "external" })
  if (v["type"] === "run")
    return Object.freeze({
      type: "run",
      runId: resourceID(v["run_id"], "Turn.source.run_id"),
    })
  throw new Error("Turn.source.type is invalid")
}
export function parseTurnState(value: unknown): TurnState {
  const v = objectValue(value, "Turn")
  if (
    !["queued", "running", "completed", "failed", "interrupted", "cancelled"].includes(
      v["status"] as string,
    )
  )
    throw new Error("Turn.status is invalid")
  return Object.freeze({
    id: resourceID(v["id"], "Turn.id"),
    sessionId: resourceID(v["session_id"], "Turn.session_id"),
    sequence: safeSequence(v["sequence"], "Turn.sequence"),
    input: jsonValue(v["input"]),
    source: parseTurnSource(v["source"]),
    status: v["status"] as TurnState["status"],
    createdAt: timestampString(v["created_at"], "Turn.created_at"),
    interruptRequested: booleanValue(
      v["interrupt_requested"],
      "Turn.interrupt_requested",
    ),
    acceptsMessages: booleanValue(
      v["accepts_messages"],
      "Turn.accepts_messages",
    ),
    ...(v["terminal_event_id"] === undefined
      ? {}
      : {
          terminalEventId: resourceID(
            v["terminal_event_id"],
            "Turn.terminal_event_id",
          ),
        }),
    ...(v["workspace_version_id"] === undefined
      ? {}
      : {
          workspaceVersionId: resourceID(
            v["workspace_version_id"],
            "Turn.workspace_version_id",
          ),
        }),
    ...(Object.hasOwn(v, "result") ? { result: jsonValue(v["result"]) } : {}),
    ...(Object.hasOwn(v, "error") ? { error: jsonValue(v["error"]) } : {}),
  })
}
export function parseSessionAdmissionReceipt(
  value: unknown,
): SessionAdmissionReceipt {
  const v = objectValue(value, "Session admission")
  const id = resourceID(v["id"], "Session admission.id"),
    turnId = resourceID(v["turn_id"], "Session admission.turn_id")
  if (v["kind"] === "enqueued")
    return Object.freeze({ id, kind: v["kind"], turnId })
  if (v["kind"] === "messaged")
    return Object.freeze({
      id,
      kind: v["kind"],
      turnId,
      messageId: resourceID(v["message_id"], "Session admission.message_id"),
    })
  throw new Error("Session admission.kind is invalid")
}
export function parseSessionMessageReceipt(
  value: unknown,
): SessionMessageReceipt {
  const v = objectValue(value, "Message receipt")
  if (v["status"] !== "accepted")
    throw new Error("Message receipt.status is invalid")
  return Object.freeze({
    id: resourceID(v["id"], "Message receipt.id"),
    turnId: resourceID(v["turn_id"], "Message receipt.turn_id"),
    messageId: resourceID(v["message_id"], "Message receipt.message_id"),
    status: v["status"],
  })
}
export function parseSessionCloseReceipt(value: unknown): SessionCloseReceipt {
  const v = objectValue(value, "Close receipt")
  if (v["status"] !== "accepted")
    throw new Error("Close receipt.status is invalid")
  return Object.freeze({
    id: resourceID(v["id"], "Close receipt.id"),
    sessionId: resourceID(v["session_id"], "Close receipt.session_id"),
    status: v["status"],
  })
}
export function parseSessionCancelReceipt(value: unknown): SessionCancelReceipt {
  const v = objectValue(value, "Cancel receipt")
  if (v["status"] !== "accepted")
    throw new Error("Cancel receipt.status is invalid")
  return Object.freeze({
    id: resourceID(v["id"], "Cancel receipt.id"),
    sessionId: resourceID(v["session_id"], "Cancel receipt.session_id"),
    status: v["status"],
  })
}
export function parseTurnInterruptReceipt(
  value: unknown,
): TurnInterruptReceipt {
  const v = objectValue(value, "Interrupt receipt")
  if (v["status"] !== "accepted")
    throw new Error("Interrupt receipt.status is invalid")
  return Object.freeze({
    id: resourceID(v["id"], "Interrupt receipt.id"),
    sessionId: resourceID(v["session_id"], "Interrupt receipt.session_id"),
    turnId: resourceID(v["turn_id"], "Interrupt receipt.turn_id"),
    holdId: resourceID(v["hold_id"], "Interrupt receipt.hold_id"),
    status: v["status"],
  })
}
export function parseSessionResumeReceipt(
  value: unknown,
): SessionResumeReceipt {
  const v = objectValue(value, "Resume receipt")
  if (v["status"] !== "accepted")
    throw new Error("Resume receipt.status is invalid")
  return Object.freeze({
    id: resourceID(v["id"], "Resume receipt.id"),
    sessionId: resourceID(v["session_id"], "Resume receipt.session_id"),
    holdId: resourceID(v["hold_id"], "Resume receipt.hold_id"),
    status: v["status"],
  })
}
export function parseSessionRecoveryReceipt(
  value: unknown,
): SessionRecoveryReceipt {
  const v = objectValue(value, "Recovery receipt")
  if (v["status"] !== "accepted")
    throw new Error("Recovery receipt.status is invalid")
  return Object.freeze({
    id: resourceID(v["id"], "Recovery receipt.id"),
    sessionId: resourceID(v["session_id"], "Recovery receipt.session_id"),
    turnId: nullableID(v["turn_id"], "Recovery receipt.turn_id"),
    holdId: resourceID(v["hold_id"], "Recovery receipt.hold_id"),
    status: v["status"],
  })
}
export function parseOutputReceipt(value: unknown): OutputReceipt {
  const v = objectValue(value, "Output receipt")
  return Object.freeze({
    id: resourceID(v["id"], "Output receipt.id"),
    sessionId: resourceID(v["session_id"], "Output receipt.session_id"),
    turnId: nullableID(v["turn_id"], "Output receipt.turn_id"),
    sequence: safeSequence(v["sequence"], "Output receipt.sequence"),
    runId: resourceID(v["run_id"], "Output receipt.run_id"),
    attemptNumber: safeSequence(
      v["attempt_number"],
      "Output receipt.attempt_number",
    ),
    runGeneration: safeSequence(
      v["run_generation"],
      "Output receipt.run_generation",
    ),
  })
}
const eventKinds: readonly SessionEventKind[] = [
  "output",
  "turn.enqueued",
  "turn.started",
  "turn.interrupt_requested",
  "turn.completed",
  "turn.failed",
  "turn.interrupted",
  "message.accepted",
  "message.handled",
  "message.rejected",
  "message.unknown",
  "session.closing",
  "session.closed",
  "session.cancel_requested",
  "turn.cancelled",
  "session.failed",
  "session.held",
  "session.resumed",
  "session.recovered",
]
export function parseSessionEvent(value: unknown): SessionEvent {
  const v = objectValue(value, "Session event")
  if (!eventKinds.includes(v["kind"] as SessionEventKind))
    throw new Error("Session event.kind is invalid")
  const p =
    v["provenance"] === null
      ? null
      : objectValue(v["provenance"], "Session event.provenance")
  return Object.freeze({
    id: resourceID(v["id"], "Session event.id"),
    sessionId: resourceID(v["session_id"], "Session event.session_id"),
    turnId: nullableID(v["turn_id"], "Session event.turn_id"),
    sequence: safeSequence(v["sequence"], "Session event.sequence"),
    createdAt: timestampString(v["created_at"], "Session event.created_at"),
    kind: v["kind"] as SessionEventKind,
    data: jsonValue(v["data"]),
    provenance:
      p === null
        ? null
        : Object.freeze({
            runId: resourceID(p["run_id"], "provenance.run_id"),
            attemptNumber: safeSequence(
              p["attempt_number"],
              "provenance.attempt_number",
            ),
            runGeneration: safeSequence(
              p["run_generation"],
              "provenance.run_generation",
            ),
            deploymentId: resourceID(
              p["deployment_id"],
              "provenance.deployment_id",
            ),
          }),
  })
}
export function parseSessionEventPage(value: unknown): SessionEventPage {
  const v = objectValue(value, "Session events")
  if (!Array.isArray(v["records"]))
    throw new Error("Session events.records must be an array")
  return Object.freeze({
    records: Object.freeze(v["records"].map(parseSessionEvent)),
    nextAfter: safeSequence(v["next_after"], "Session events.next_after"),
    hasMore: booleanValue(v["has_more"], "Session events.has_more"),
    retainedAfter: safeSequence(
      v["retained_after"],
      "Session events.retained_after",
    ),
  })
}
function parseSessionFailure(value: unknown): SessionFailure {
  const v = objectValue(value, "Session failure"),
    d = objectValue(v["details"], "Session failure.details")
  return Object.freeze({
    code: requiredString(v["code"], "Session failure.code"),
    message: requiredString(v["message"], "Session failure.message"),
    details: Object.freeze(
      d["run_id"] === undefined
        ? {}
        : { runId: resourceID(d["run_id"], "Session failure.details.run_id") },
    ),
  })
}
export function sessionStatus(
  value: unknown,
  label = "Session.status",
): SessionStatus {
  if (
    value !== "open" &&
    value !== "closing" &&
    value !== "closed" &&
    value !== "failed"
  )
    throw new Error(`${label} is invalid`)
  return value
}
function nullableID(value: unknown, label: string): string | null {
  return value === null ? null : resourceID(value, label)
}
function jsonValue(value: unknown): JsonValue {
  canonicalizeJsonValue(value as JsonValue)
  return value as JsonValue
}
function requiredString(value: unknown, label: string): string {
  if (typeof value !== "string" || value.length === 0)
    throw new Error(`${label} must be a non-empty string`)
  return value
}
function booleanValue(value: unknown, label: string): boolean {
  if (typeof value !== "boolean") throw new Error(`${label} must be a boolean`)
  return value
}
function objectValue(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value))
    throw new Error(`${label} must be an object`)
  return value as Record<string, unknown>
}
function safeSequence(value: unknown, label: string): number {
  if (!Number.isSafeInteger(value) || (value as number) < 0)
    throw new Error(`${label} must be a non-negative safe integer`)
  return value as number
}
