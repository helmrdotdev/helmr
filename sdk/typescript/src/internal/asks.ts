import type { AskPage, AskState, AskAnswerReceipt, JsonValue } from "../contract"
import { canonicalizeJsonValue } from "./jsoncanon"
import { normalizeQuestion, normalizeAnswer } from "../question"
import { resourceID } from "./id"
import { timestampString } from "./timestamp"

function object(value: unknown): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) throw new Error("Invalid ask resource")
  return value as Record<string, unknown>
}
function parse(value: unknown, receipt: boolean): AskState | AskAnswerReceipt {
  const v = object(value), status = v["status"]
  if (status !== "pending" && status !== "answered" && status !== "cancelled") throw new Error("Invalid ask status")
  const expired = v["payload_expired"] === true
  if (v["payload_expired"] !== undefined && !expired) throw new Error("Invalid ask expiry")
  const result = {
    id: resourceID(v["id"], "Ask.id"), sessionId: resourceID(v["session_id"], "Ask.session_id"), turnId: resourceID(v["turn_id"], "Ask.turn_id"), status,
    createdAt: timestampString(v["created_at"], "Ask.created_at"),
    ...(v["answered_at"] === undefined ? {} : { answeredAt: timestampString(v["answered_at"], "Ask.answered_at") }),
    ...(v["cancelled_at"] === undefined ? {} : { cancelledAt: timestampString(v["cancelled_at"], "Ask.cancelled_at") }),
    ...(v["answered_by_user_id"] === undefined ? {} : { answeredByUserId: resourceID(v["answered_by_user_id"], "Ask.answered_by_user_id") }),
    ...(v["answered_by_api_key_id"] === undefined ? {} : { answeredByApiKeyId: resourceID(v["answered_by_api_key_id"], "Ask.answered_by_api_key_id") }),
    ...(expired ? { payloadExpired: true } : {}),
  }
  const responders = Number(result.answeredByUserId !== undefined) + Number(result.answeredByApiKeyId !== undefined)
  if ((status === "answered" ? responders !== 1 : responders !== 0) || (status === "answered") !== (result.answeredAt !== undefined) || (status === "cancelled") !== (result.cancelledAt !== undefined)) throw new Error("Invalid ask attribution or terminal time")
  if (receipt && status !== "answered") throw new Error("Invalid ask answer receipt status")
  if (expired) {
    if (status === "pending" || ["question", "answer"].some(key => Object.hasOwn(v, key))) throw new Error("Invalid expired ask payload")
    return Object.freeze(result) as AskState | AskAnswerReceipt
  }
  if (receipt) {
    if (status !== "answered" || !Object.hasOwn(v, "answer") || Object.hasOwn(v, "question")) throw new Error("Invalid ask response receipt")
    canonicalizeJsonValue(v["answer"] as JsonValue)
    return Object.freeze({ ...result, answer: v["answer"] as JsonValue }) as AskAnswerReceipt
  }
  const question = normalizeQuestion(v["question"])
  if ((status === "answered") !== Object.hasOwn(v, "answer")) throw new Error("Invalid ask answer presence")
  return Object.freeze({ ...result, question, ...(status === "answered" ? { answer: normalizeAnswer(question, v["answer"]) } : {}) }) as AskState
}
export function parseAsk(value: unknown): AskState { return parse(value, false) as AskState }
export function parseAskReceipt(value: unknown): AskAnswerReceipt { return parse(value, true) as AskAnswerReceipt }
export function parseAskPage(value: unknown): AskPage {
  const v = object(value)
  if (!Array.isArray(v["asks"]) || (v["next_cursor"] !== undefined && (typeof v["next_cursor"] !== "string" || !v["next_cursor"]))) throw new Error("Invalid ask page")
  return Object.freeze({ items: Object.freeze(v["asks"].map(parseAsk)), ...(v["next_cursor"] === undefined ? {} : { nextCursor: v["next_cursor"] as string }) })
}
