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
    case "session.storage_pressure":
      if (data["state"] !== "paused" && data["state"] !== "resolved" && data["state"] !== "stopped") throw new Error("Invalid storage pressure state")
      return {computerId:id("computerId"),leaseEpoch:sequence("leaseEpoch"),holdId:id("holdId"),state:data["state"]}
    case "session.recovery_gap": {
      if (!Array.isArray(data["affectedTurnIds"]) || !Number.isSafeInteger(data["affectedTurnCount"]) || data["affectedTurnCount"] !== data["affectedTurnIds"].length) throw new Error("Invalid recovery gap affected Turns")
      if (data["restoredAt"] !== null && (typeof data["restoredAt"] !== "string" || Number.isNaN(Date.parse(data["restoredAt"])))) throw new Error("Invalid recovery gap restore timestamp")
      if ((data["recoverySaveId"]===null)!==(data["restoredAt"]===null)) throw new Error("Recovery gap restore point is inconsistent")
      return {holdId:id("holdId"),computerId:id("computerId"),leaseEpoch:sequence("leaseEpoch"),recoverySaveId:data["recoverySaveId"]===null?null:id("recoverySaveId"),restoredAt:data["restoredAt"] as string|null,affectedTurnIds:data["affectedTurnIds"].map(v=>resourceID(v,"affected Turn")),affectedTurnCount:data["affectedTurnCount"] as number}
    }
    case "session.process_failed": return { holdId: id("holdId"), processEpoch: sequence("processEpoch") }
    case "session.deadline": return { holdId: id("holdId") }
    case "session.close":
      if (data["reason"] === "close_after_idle") return { reason: "close_after_idle" }
      return { controlId: id("controlId"), ...(data["holdId"] === undefined ? {} : { holdId: id("holdId") }) }
    case "session.interrupt": case "session.resume": case "session.cancel": case "turn.interrupt":
      return { controlId: id("controlId"), ...(data["holdId"] === undefined ? {} : { holdId: id("holdId") }) }
    case "turn.failed": return data["code"] === undefined ? {} : { code: text("code") }
    case "turn.interrupted":
      if (Object.keys(data).length === 0) return {}
      if (Object.keys(data).length !== 4 || data["reason"] !== "computer_execution_lost" || typeof data["recoveryGap"] !== "boolean") throw new Error("Invalid Computer loss interruption data")
      return { reason: "computer_execution_lost", computerId: id("computerId"), leaseEpoch: sequence("leaseEpoch"), recoveryGap: data["recoveryGap"] }
    default:
      if (Object.keys(data).length !== 0) throw new Error("Lifecycle event.data must be empty")
      return {}
  }
}
