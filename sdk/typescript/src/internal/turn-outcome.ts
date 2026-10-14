import type { TurnOutcome } from "../contract"
import { normalizeContent } from "../content"
import { normalizeJsonValue } from "./jsoncanon"
import { timestampString } from "./timestamp"

export function parseTurnOutcome(value: unknown): TurnOutcome {
  if (value === null || typeof value !== "object" || Array.isArray(value)) throw new Error("Invalid Turn outcome")
  const v = value as Record<string, unknown>
  const status = v["status"]
  const expired = v["payloadExpiredAt"] === undefined ? undefined : timestampString(v["payloadExpiredAt"], "Turn.payloadExpiredAt")
  if (status !== "completed" && status !== "failed" && status !== "interrupted" && status !== "cancelled") throw new Error("Invalid Turn outcome status")
  if ((status !== "completed" || expired !== undefined) && (v["result"] !== undefined || v["response"] !== undefined)) throw new Error("Turn outcome cannot contain a payload")
  if (status !== "failed" && v["error"] !== undefined) throw new Error("Only failed Turns contain an error")
  if (status === "completed") {
    if (expired !== undefined) return { status, payloadExpiredAt: expired }
    if (!Object.hasOwn(v, "result")) throw new Error("Completed Turn.result is required")
    return { status, result: normalizeJsonValue(v["result"]), ...(v["response"] === undefined ? {} : { response: normalizeContent(v["response"], false) }) }
  }
  const retention = expired === undefined ? {} : { payloadExpiredAt: expired }
  if (status !== "failed") return { status, ...retention }
  if (v["error"] === undefined) return { status, ...retention }
  const error = v["error"]
  if (error === null || typeof error !== "object" || Array.isArray(error)) throw new Error("Invalid Turn error")
  const e = error as Record<string, unknown>
  if (typeof e["code"] !== "string" || !e["code"]) throw new Error("Invalid Turn error code")
  if (expired !== undefined) {
    if (e["message"] !== undefined) throw new Error("Expired Turn cannot contain an error message")
    return { status, payloadExpiredAt: expired, error: { code: e["code"] } }
  }
  if (typeof e["message"] !== "string") throw new Error("Invalid Turn error message")
  return { status, error: { code: e["code"], message: e["message"] } }
}
