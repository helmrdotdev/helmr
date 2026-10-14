import type { SessionEvent, SessionEventKind } from "../contract"
import { normalizeContent } from "../content"
import { normalizeJsonValue } from "./jsoncanon"
import { normalizeQuestion } from "../question"
import { resourceID } from "./id"

export function parseSessionEventData(kind: SessionEventKind, value: unknown): SessionEvent["data"] {
  if (kind === "turn.output") return normalizeContent(value, false)
  const canonical = normalizeJsonValue(value)
  if (canonical === null || typeof canonical !== "object" || Array.isArray(canonical)) throw new Error("Session event.data must be an object")
  const data = canonical as Record<string, unknown>
  const id = (key: string) => resourceID(data[key], `Session event.data.${key}`)
  const text = (key: string) => {
    if (typeof data[key] !== "string" || !data[key]) throw new Error(`Invalid Session event.data.${key}`)
    return data[key] as string
  }
  const sequence = (key: string) => {
    if (typeof data[key] !== "number" || !Number.isSafeInteger(data[key]) || data[key] < 1) throw new Error(`Invalid Session event.data.${key}`)
    return data[key] as number
  }
  switch (kind) {
    case "ask.created": return { ask_id: id("ask_id"), question: normalizeQuestion(data["question"]) }
    case "ask.cancelled": return { ask_id: id("ask_id") }
    case "ask.answered": {
      if ((data["answered_by_user_id"] === undefined) === (data["answered_by_api_key_id"] === undefined)) throw new Error("Answered event requires one verified actor")
      return { ask_id: id("ask_id"), answer: normalizeJsonValue(data["answer"]), ...(data["answered_by_user_id"] === undefined ? { answered_by_api_key_id: id("answered_by_api_key_id") } : { answered_by_user_id: id("answered_by_user_id") }) }
    }
    case "message.admitted": case "message.started": case "message.delivered": case "message.rejected": {
      const status = kind.slice("message.".length)
      const delivery = data["delivery"]
      if (data["status"] !== status || (status === "rejected" ? delivery !== "not_delivered" && delivery !== "uncertain" : delivery !== status)) throw new Error("Message event status and delivery disagree")
      return { message_id: id("message_id"), status, delivery, ...(data["reason"] === undefined ? {} : { reason: text("reason") }) } as SessionEvent["data"]
    }
    case "slack.delivery_unavailable":
      if (data["reason"] !== "route_unavailable") throw new Error("Invalid Slack delivery event reason")
      return { reason: "route_unavailable", source_seq: sequence("source_seq") }
    case "session.process_failed": return { holdId: id("holdId"), processEpoch: sequence("processEpoch") }
    case "session.deadline": return { holdId: id("holdId") }
    case "session.close":
      if (data["reason"] === "close_after_idle") return { reason: "close_after_idle" }
      return { controlId: id("controlId"), ...(data["holdId"] === undefined ? {} : { holdId: id("holdId") }) }
    case "session.interrupt": case "session.resume": case "session.cancel": case "turn.interrupt":
      return { controlId: id("controlId"), ...(data["holdId"] === undefined ? {} : { holdId: id("holdId") }) }
    case "turn.failed": return data["code"] === undefined ? {} : { code: text("code") }
    default:
      if (Object.keys(data).length !== 0) throw new Error("Lifecycle event.data must be empty")
      return {}
  }
}
