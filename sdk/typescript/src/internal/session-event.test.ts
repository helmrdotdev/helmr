import { expect, test } from "bun:test"
import { parseSessionEventData } from "./session-event"
import { parseTurnOutcome } from "./turn-outcome"
import { isHelmrError, ContentError } from "../index"
const id = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34"

test("events validate their producer-specific payload before exposing narrowed data", () => {
  expect(parseSessionEventData("turn.completed", {})).toEqual({})
  expect(() => parseSessionEventData("turn.completed", { result: 1 })).toThrow()
  expect(parseSessionEventData("message.rejected", { message_id: id, status: "rejected", delivery: "uncertain", reason: "process_lost" })).toMatchObject({ delivery: "uncertain" })
  expect(() => parseSessionEventData("message.delivered", { message_id: id, status: "delivered", delivery: "uncertain" })).toThrow()
  const answer = { ask_id: id, answer: { selected: [{ id: "allow", value: true }] }, answered_by_user_id: id }
  expect(parseSessionEventData("ask.answered", answer)).toEqual(answer)
  expect(() => parseSessionEventData("ask.answered", { ...answer, answered_by_api_key_id: id })).toThrow()
  expect(parseSessionEventData("session.close", { reason: "close_after_idle" })).toEqual({ reason: "close_after_idle" })
  expect(parseSessionEventData("session.resume", { controlId: id, holdId: id })).toEqual({ controlId: id, holdId: id })
})

test("Turn outcome distinguishes a null result from expired payload and impossible states", () => {
  expect(parseTurnOutcome({ status: "completed", result: null })).toEqual({ status: "completed", result: null })
  const expired = { status: "completed", payloadExpiredAt: "2026-10-13T00:00:00Z" }
  expect(parseTurnOutcome(expired)).toEqual(expired)
  for (const invalid of [{ status: "completed" }, { ...expired, result: null }, { status: "failed", result: 1 }, { status: "interrupted", error: { code: "failure", message: "x" } }]) expect(() => parseTurnOutcome(invalid)).toThrow()
})

test("coded SDK failures can be narrowed without wrapping native failures", () => {
  expect(isHelmrError(new ContentError("invalid_arguments", "bad content"))).toBe(true)
  expect(isHelmrError(Object.assign(new Error("capacity"), { code: "capacity_wait_blocked" }))).toBe(true)
  expect(isHelmrError(new Error("network"))).toBe(false)
  expect(isHelmrError(new DOMException("aborted", "AbortError"))).toBe(false)
})


test("retained results and event envelopes do not inherit invocation size limits", () => {
  const result = { report: "x".repeat(300 * 1024) }
  expect(parseTurnOutcome({ status: "completed", result })).toEqual({ status: "completed", result })
  expect(() => parseTurnOutcome({ status: "completed", result: { invalid: Infinity } })).toThrow()
  const question = { type: "text", prompt: [{ type: "text", text: "" }] }
  question.prompt[0]!.text = "x".repeat(256 * 1024 - JSON.stringify(question).length)
  const event = { ask_id: id, question }
  expect(JSON.stringify(question).length).toBe(256 * 1024)
  expect(parseSessionEventData("ask.created", event)).toEqual(event)
})
