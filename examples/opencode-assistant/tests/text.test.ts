import { expect, test } from "bun:test"
import type { Event } from "@opencode-ai/sdk/v2"
import { OpenCodeText } from "../agents/text"

const message = (id: string, parentID = "prompt", extra = {}) => ({ type: "message.updated", properties: { sessionID: "session", info: { id, role: "assistant", sessionID: "session", parentID, time: { created: 1 }, ...extra } } }) as Event
const part = (id: string, messageID: string, text: string, extra = {}) => ({ type: "message.part.updated", properties: { sessionID: "session", part: { id, messageID, sessionID: "session", type: "text", text, time: { start: 1, end: 2 }, ...extra } } }) as Event

test("only owned completed assistant parts become intermediate output", () => {
  const output: string[] = []
  const text = new OpenCodeText("session", "prompt", value => output.push(value))
  text.observe(message("old", "earlier-prompt")); text.observe(part("private", "old", "Old work"))
  text.observe(message("first"))
  text.observe(part("one", "first", "Checking", { time: { start: 1 } }))
  text.observe(part("one", "first", "Checking files."))
  text.observe(part("two", "first", "Using the current revision."))
  text.observe(part("hidden", "first", "Synthetic", { synthetic: true }))
  text.observe(part("reason", "first", "Private reasoning", { type: "reasoning" }))
  expect(output).toEqual([])
  text.observe({ type: "message.part.updated", properties: { sessionID: "session", part: { id: "tool", sessionID: "session", messageID: "first", type: "tool" } } } as Event)
  text.observe(part("one", "first", "Checking files."))
  text.observe(message("final")); text.observe(part("final-text", "final", "Final response"))
  text.finish("final")
  expect(output).toEqual(["Checking files.", "Using the current revision."])
})

test("failure flush keeps verified complete text while excluding partial or foreign content", () => {
  const output: string[] = []
  const text = new OpenCodeText("session", "prompt", value => output.push(value))
  text.observe(message("one")); text.observe(part("complete", "one", "Known explanation"))
  text.observe(part("partial", "one", "unfinished", { time: { start: 1 } }))
  text.observe(part("foreign", "one", "wrong session", { sessionID: "another" }))
  text.flush()
  expect(output).toEqual(["Known explanation"])
})
