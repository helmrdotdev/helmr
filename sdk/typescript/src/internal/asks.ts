import type { AskPage, AskState, AskResponseReceipt, JsonValue } from "../contract"
import { normalizeQuestion, normalizeAnswer } from "../question"
import { resourceID } from "./id"
import { timestampString } from "./timestamp"

function object(value: unknown): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) throw new Error("Invalid ask resource")
  return value as Record<string, unknown>
}
function parse(value: unknown, receipt: boolean): AskState {
  const v = object(value), status = v["status"]
  if (status !== "pending" && status !== "responded" && status !== "cancelled") throw new Error("Invalid ask status")
  const expired = v["payload_expired"] === true
  if (v["payload_expired"] !== undefined && !expired) throw new Error("Invalid ask expiry")
  const result: AskState = {
    id: resourceID(v["id"], "Ask.id"), sessionId: resourceID(v["session_id"], "Ask.session_id"), turnId: resourceID(v["turn_id"], "Ask.turn_id"), status,
    createdAt: timestampString(v["created_at"], "Ask.created_at"),
    ...(v["responded_at"] === undefined ? {} : { respondedAt: timestampString(v["responded_at"], "Ask.responded_at") }),
    ...(v["cancelled_at"] === undefined ? {} : { cancelledAt: timestampString(v["cancelled_at"], "Ask.cancelled_at") }),
    ...(v["responded_by_user_id"] === undefined ? {} : { respondedByUserId: resourceID(v["responded_by_user_id"], "Ask.responded_by_user_id") }),
    ...(v["responded_by_api_key_id"] === undefined ? {} : { respondedByApiKeyId: resourceID(v["responded_by_api_key_id"], "Ask.responded_by_api_key_id") }),
    ...(expired ? { payloadExpired: true } : {}),
  }
  const responders = Number(result.respondedByUserId !== undefined) + Number(result.respondedByApiKeyId !== undefined)
  if ((status === "responded" ? responders !== 1 : responders !== 0) || (status === "responded") !== (result.respondedAt !== undefined) || (status === "cancelled") !== (result.cancelledAt !== undefined)) throw new Error("Invalid ask attribution or terminal time")
  if (expired) {
    if (status === "pending" || ["prompt", "answer_control", "answer"].some(key => Object.hasOwn(v, key))) throw new Error("Invalid expired ask payload")
    return Object.freeze(result)
  }
  if (receipt) {
    if (status !== "responded" || !Object.hasOwn(v, "answer") || Object.hasOwn(v, "prompt") || Object.hasOwn(v, "answer_control")) throw new Error("Invalid ask response receipt")
    return Object.freeze({ ...result, answer: v["answer"] as JsonValue })
  }
  const question = normalizeQuestion({ prompt: v["prompt"], answer: v["answer_control"] })
  if ((status === "responded") !== Object.hasOwn(v, "answer")) throw new Error("Invalid ask answer presence")
  return Object.freeze({ ...result, prompt: question.prompt, answerControl: question.answer, ...(status === "responded" ? { answer: normalizeAnswer(question, v["answer"]) } : {}) })
}
export function parseAsk(value: unknown): AskState { return parse(value, false) }
export function parseAskReceipt(value: unknown): AskResponseReceipt { return parse(value, true) }
export function parseAskPage(value: unknown): AskPage {
  const v = object(value)
  if (!Array.isArray(v["asks"]) || (v["next_cursor"] !== undefined && (typeof v["next_cursor"] !== "string" || !v["next_cursor"]))) throw new Error("Invalid ask page")
  return Object.freeze({ asks: Object.freeze(v["asks"].map(parseAsk)), ...(v["next_cursor"] === undefined ? {} : { nextCursor: v["next_cursor"] as string }) })
}
