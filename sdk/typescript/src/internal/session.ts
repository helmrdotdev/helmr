import type {
  JsonValue,
  Session,
  SessionStatus,
  CursorPage,
  TurnState,
  SessionAdmissionReceipt,
  SessionMessageReceipt,
  SessionCloseReceipt,
  SessionCancelReceipt,
  SessionInterruptReceipt,
  SessionResumeReceipt,
  SessionEvent,
  SessionEventKind,
  SessionEventPage,
} from "../contract"
import { normalizeContent, normalizeInput } from "../content"
import { canonicalizeJsonValue } from "./jsoncanon"
import { resourceID } from "./id"
import { slackStartOption } from "../client-slack"
import { timestampString } from "./timestamp"

export function parseSession(value: unknown): Session {
  const v = objectValue(value, "Session")
  const initial = v["initial_turn"] === null ? null : objectValue(v["initial_turn"], "Session.initial_turn")
  if (!Array.isArray(v["holds"]))
    throw new Error("Session.holds must be an array")
  return Object.freeze({
    id: resourceID(v["id"], "Session.id"),
    agentId: resourceID(v["agent_id"], "Session.agent_id"),
    deploymentId: resourceID(v["deployment_id"], "Session.deployment_id"),
    computerId: resourceID(v["computer_id"], "Session.computer_id"),
    rootSessionId: resourceID(v["root_session_id"], "Session.root_session_id"),
    ...(v["slack_channel_id"] === undefined ? {} : { slackChannelId: slackStartOption({ channelId: v["slack_channel_id"] }).channel_id }),
    parentSessionId: nullableID(
      v["parent_session_id"],
      "Session.parent_session_id",
    ),
    requesterSessionId: nullableID(v["requester_session_id"], "Session.requester_session_id"),
    initialTurn: initial === null ? null : Object.freeze({ id: resourceID(initial["id"], "Session.initial_turn.id"), status: turnStatus(initial["status"]) }),
    ...(v["key"] === undefined
      ? {}
      : { key: requiredString(v["key"], "Session.key") }),
    status: sessionStatus(v["status"]),
    createdAt: timestampString(v["created_at"], "Session.created_at"),
    holds: Object.freeze(
      v["holds"].map((value) => {
        const h = objectValue(value, "Session hold")
        if (h["scope"] !== "local" && h["scope"] !== "subtree")
          throw new Error("Session hold.scope is invalid")
        if (typeof h["reason"] !== "string")
          throw new Error("Session hold.reason is invalid")
        return Object.freeze({
          id: resourceID(h["id"], "Session hold.id"),
          sessionId: resourceID(h["session_id"], "Session hold.session_id"),
          scope: h["scope"],
          reason: h["reason"],
          createdAt: timestampString(
            h["created_at"],
            "Session hold.created_at",
          ),
        })
      }),
    ),
  })
}
export function parseSessionPage(value: unknown): CursorPage<Session> {
  const page = objectValue(value, "Session list response")
  if (!Array.isArray(page["sessions"])) throw new Error("Session list response.sessions must be an array")
  return Object.freeze({
    items: Object.freeze(page["sessions"].map(parseSession)),
    ...(page["next_cursor"] === undefined ? {} : { nextCursor: resourceID(page["next_cursor"], "Session list response.next_cursor") }),
  })
}

function turnStatus(value: unknown): TurnState["status"] {
  if (typeof value !== "string" || !["queued", "running", "finalizing", "completed", "failed", "interrupted", "cancelled"].includes(value)) throw new Error("Turn.status is invalid")
  return value as TurnState["status"]
}

export function parseTurnState(value: unknown): TurnState {
  const v = objectValue(value, "Turn")
  const status = turnStatus(v["status"])
  const terminal = ["completed", "failed", "interrupted", "cancelled"].includes(
    v["status"] as string,
  )
  if (terminal !== (v["terminal_at"] !== undefined))
    throw new Error("Turn terminal timestamp is inconsistent")
  if ((v["status"] === "completed") !== (v["completion_save_id"] !== undefined))
    throw new Error("Turn completion save is inconsistent")
  if ((Object.hasOwn(v, "result") || Object.hasOwn(v, "response")) && v["status"] !== "completed")
    throw new Error("An unfinished Turn cannot publish a result")
  const expired = v["payload_expired_at"] !== undefined
  if (expired && (!terminal || ["input", "result", "response"].some(key => Object.hasOwn(v, key))))
    throw new Error("Expired Turn payload is inconsistent")
  if (!expired && !Object.hasOwn(v, "input")) throw new Error("Turn.input is required")
  const failure = v["error"] === undefined ? undefined : objectValue(v["error"], "Turn.error")
  if (failure !== undefined && (v["status"] !== "failed" || typeof failure["code"] !== "string" || !failure["code"] || (expired ? failure["message"] !== undefined : typeof failure["message"] !== "string")))
    throw new Error("Turn.error is invalid")
  return Object.freeze({
    ...(failure === undefined ? {} : { error: Object.freeze({ code: failure["code"] as string, ...(expired ? {} : { message: failure["message"] as string }) }) }),
    id: resourceID(v["id"], "Turn.id"),
    sessionId: resourceID(v["session_id"], "Turn.session_id"),
    sequence: safeSequence(v["sequence"], "Turn.sequence"),
    ...(expired ? { payloadExpiredAt: timestampString(v["payload_expired_at"], "Turn.payload_expired_at") } : { input: normalizeInput(v["input"]) }),
    status,
    ...(v["started_at"] === undefined
      ? {}
      : { startedAt: timestampString(v["started_at"], "Turn.started_at") }),
    ...(v["terminal_at"] === undefined
      ? {}
      : { terminalAt: timestampString(v["terminal_at"], "Turn.terminal_at") }),
    ...(v["completion_save_id"] === undefined
      ? {}
      : {
          completionSaveId: resourceID(
            v["completion_save_id"],
            "Turn.completion_save_id",
          ),
        }),
    ...(Object.hasOwn(v, "response") ? { response: normalizeContent(v["response"], false) } : {}),
    ...(Object.hasOwn(v, "result") ? { result: jsonValue(v["result"]) } : {}),
  })
}
export function parseTurnAdmission(
  value: unknown,
): Readonly<{ sessionId: string; turnId: string; sequence: number }> {
  const v = objectValue(value, "Turn admission")
  return Object.freeze({
    sessionId: resourceID(v["session_id"], "Turn admission.session_id"),
    turnId: resourceID(v["turn_id"], "Turn admission.turn_id"),
    sequence: safeSequence(v["sequence"], "Turn admission.sequence"),
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
export function parseSessionCancelReceipt(
  value: unknown,
): SessionCancelReceipt {
  const v = objectValue(value, "Cancel receipt")
  if (v["status"] !== "accepted")
    throw new Error("Cancel receipt.status is invalid")
  return Object.freeze({
    id: resourceID(v["id"], "Cancel receipt.id"),
    sessionId: resourceID(v["session_id"], "Cancel receipt.session_id"),
    status: v["status"],
  })
}
export function parseSessionInterruptReceipt(
  value: unknown,
): SessionInterruptReceipt {
  const v = objectValue(value, "Interrupt receipt")
  if (v["status"] !== "accepted")
    throw new Error("Interrupt receipt.status is invalid")
  return Object.freeze({
    id: resourceID(v["id"], "Interrupt receipt.id"),
    sessionId: resourceID(v["session_id"], "Interrupt receipt.session_id"),
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
const eventKinds: readonly SessionEventKind[] = [
  "message.admitted", "message.started", "message.delivered", "message.rejected",
  "slack.delivery_unavailable",
  "ask.created", "ask.responded", "ask.cancelled",
  "turn.output",
  "turn.queued",
  "turn.running",
  "turn.finalizing",
  "turn.completed",
  "turn.failed",
  "turn.interrupted",
  "turn.cancelled",
  "session.cancelled",
  "session.closed",
  "session.process_stopped",
  "session.process_failed",
  "session.deadline",
  "session.interrupt",
  "session.resume",
  "session.close",
  "session.cancel",
]
export function parseSessionEvent(value: unknown): SessionEvent {
  const v = objectValue(value, "Session event")
  if (!eventKinds.includes(v["kind"] as SessionEventKind))
    throw new Error("Session event.kind is invalid")
  return Object.freeze({
    sessionId: resourceID(v["session_id"], "Session event.session_id"),
    turnId: nullableID(v["turn_id"], "Session event.turn_id"),
    sequence: safeSequence(v["sequence"], "Session event.sequence"),
    createdAt: timestampString(v["created_at"], "Session event.created_at"),
    kind: v["kind"] as SessionEventKind,
    data: jsonValue(v["data"]),
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
export function sessionStatus(
  value: unknown,
  label = "Session.status",
): SessionStatus {
  if (
    value !== "open" &&
    value !== "closing" &&
    value !== "closed" &&
    value !== "cancelled"
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
