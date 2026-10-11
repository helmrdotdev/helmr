import { expect, test } from "bun:test"
import { ANSWER_BYTES, CHOICE_TEXT_BYTES, normalizeAnswer, normalizeQuestion, type Question } from "./question"

const question = (multiple = true, allowText = true): Question => ({
  prompt: [{ type: "text", text: "choose\0雪" }],
  answer: { type: "choice", multiple, allowText, options: [
    { id: "one", label: "First", value: { z: 1, a: null } },
    { id: "two", label: "Second", value: false },
  ] },
})
test("questions preserve exact selected values, order and ordinary free text", () => {
  const q = normalizeQuestion(question())
  for (const answer of [
    { selected: [{ id: "one", value: { a: null, z: 1.0 } }] },
    { selected: [{ id: "one", value: { a: null, z: 1 } }, { id: "two", value: false }], text: "feedback" },
    { selected: [], text: "Allow" },
  ]) expect(normalizeAnswer(q, answer)).toEqual(answer)
  for (const answer of [
    { selected: [], text: "" }, { selected: null, text: "yes" },
    { selected: [{ id: "two", value: false }, { id: "one", value: { a: null, z: 1 } }] },
    { selected: [{ id: "two", value: false }, { id: "two", value: false }] },
    { selected: [{ id: "two", value: true }] }, { selected: [{ id: "unknown", value: false }] },
    { selected: [{ id: "two" }] }, { selected: [{ id: "two", value: false }], respondedBy: { kind: "user", id: "fake" } },
  ]) expect(() => normalizeAnswer(q, answer)).toThrow()
  expect(normalizeAnswer({ prompt: [], answer: { type: "text" } }, "")).toBe("")
  expect(() => normalizeAnswer(question(false, false), { selected: [{ id: "two", value: false }], text: "feedback" })).toThrow()
})
test("question admission reserves enough bytes for any configured answer", () => {
  const q = question()
  expect(() => normalizeAnswer(q, { selected: [], text: "x".repeat(CHOICE_TEXT_BYTES - 2) })).not.toThrow()
  expect(() => normalizeAnswer(q, { selected: [], text: "x".repeat(CHOICE_TEXT_BYTES - 1) })).toThrow("8 KiB")
  const large = (allowText: boolean, count: number): Question => ({ prompt: [], answer: { type: "choice", allowText, options: [{ id: "one", label: "First", value: "x".repeat(count) }] } })
  expect(() => normalizeQuestion(large(false, ANSWER_BYTES))).toThrow("64 KiB")
  expect(() => normalizeQuestion(large(false, ANSWER_BYTES - 4096))).not.toThrow()
  expect(() => normalizeQuestion(large(true, ANSWER_BYTES - 4096))).toThrow("64 KiB")
  for (const control of [{ type: "text", options: [] }, { type: "unknown" }, { type: "choice", options: [] }, { type: "choice", options: [{ id: "a", label: "A", value: null }], allowText: null }])
    expect(() => normalizeQuestion({ prompt: [], answer: control })).toThrow()
})

test("choice text size takes precedence over malformed envelopes and selections", () => {
 for (const shape of [{ selected: null }, {}, { selected: [], extra: true }, { selected: [{ id: "one", value: null }, { id: "two", value: false }] }]) {
  expect(() => normalizeAnswer(question(false, false), { ...shape, text: "x".repeat(CHOICE_TEXT_BYTES - 1) })).toThrow("8 KiB")
 }
})
test("largest configured combined choice answer fits exactly 64 KiB", () => {
  const text = "x".repeat(CHOICE_TEXT_BYTES - 2)
  const fixed = new TextEncoder().encode(JSON.stringify({ selected: [{ id: "one", value: "" }], text })).length
  const value = "x".repeat(ANSWER_BYTES - fixed)
  const q: Question = { prompt: [], answer: { type: "choice", allowText: true, options: [{ id: "one", label: "One", value }] } }
  expect(() => normalizeQuestion(q)).not.toThrow()
  expect(() => normalizeAnswer(q, { selected: [{ id: "one", value }], text })).not.toThrow()
  expect(() => normalizeQuestion({ ...q, answer: { type: "choice", allowText: true, options: [{ id: "one", label: "One", value: value + "x" }] } })).toThrow("64 KiB")
})
