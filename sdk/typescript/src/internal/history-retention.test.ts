import { expect, test } from "bun:test"
import { parseTurnState } from "./session"
const id = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33"
const at = "2026-10-07T00:00:00Z"
const retained = { id, session_id: id, sequence: 1, status: "completed", terminal_at: at, completion_save_id: id, input: [], result: null }
test("expired Turn omits content while retaining successful attribution", () => {
  expect(parseTurnState(retained)).toMatchObject({ input: [], result: null })
  const { input, result, ...identity } = retained
  const expired = parseTurnState({ ...identity, payload_expired_at: at })
  expect(expired.status).toBe("completed")
  expect(expired.completionSaveId).toBe(id)
  expect(expired.payloadExpiredAt).toBe(at)
  expect(Object.hasOwn(expired, "input")).toBe(false)
  expect(Object.hasOwn(expired, "result")).toBe(false)
  expect(() => parseTurnState(identity)).toThrow("input")
  expect(() => parseTurnState({ ...retained, payload_expired_at: at })).toThrow("Expired")
})
test("expired failures preserve code without manufacturing an empty message", () => {
  const failure = { id, session_id: id, sequence: 1, status: "failed", terminal_at: at, payload_expired_at: at, error: { code: "application_error" } }
  expect(parseTurnState(failure).error).toEqual({ code: "application_error" })
  expect(() => parseTurnState({ ...failure, error: { code: "application_error", message: "" } })).toThrow("error")
  expect(() => parseTurnState({ ...failure, status: "running", terminal_at: undefined })).toThrow("Expired")
})
