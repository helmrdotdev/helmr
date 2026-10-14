import { expect, test } from "bun:test"
import { ContentError, validateQuestion } from "./index"
import { ANSWER_BYTES, CHOICE_TEXT_BYTES, normalizeAnswer, normalizeQuestion, normalizeSubmission, type Question } from "./question"

const question = (multiple = true, allowCustom = true): Question => ({
  prompt: [{ type: "text", text: "choose\0雪" }],
   type: "choice", multiple, allowCustom, options: [
    { id: "one", label: "First", value: { z: 1, a: null } },
    { id: "two", label: "Second", value: false },
  ],
})
test("questions preserve exact selected values, order and ordinary free text", () => {
  const q = normalizeQuestion(question())
  for (const answer of [
    { selected: [{ id: "one", value: { a: null, z: 1.0 } }] },
    { selected: [{ id: "one", value: { a: null, z: 1 } }, { id: "two", value: false }], custom: "feedback" },
    { selected: [], custom: "Allow" },
  ]) expect(normalizeAnswer(q, answer)).toEqual(answer)
  for (const answer of [
    { selected: [], custom: "" }, { selected: null, custom: "yes" },
    { selected: [{ id: "two", value: false }, { id: "one", value: { a: null, z: 1 } }] },
    { selected: [{ id: "two", value: false }, { id: "two", value: false }] },
    { selected: [{ id: "two", value: true }] }, { selected: [{ id: "unknown", value: false }] },
    { selected: [{ id: "two" }] }, { selected: [{ id: "two", value: false }], answeredBy: { kind: "user", id: "fake" } },
  ]) expect(() => normalizeAnswer(q, answer)).toThrow()
  expect(normalizeAnswer({ prompt: [],  type: "text" }, "")).toBe("")
  expect(() => normalizeAnswer(question(false, false), { selected: [{ id: "two", value: false }], custom: "feedback" })).toThrow()
})
test("question admission reserves enough bytes for any configured answer", () => {
  const q = question()
  expect(() => normalizeAnswer(q, { selected: [], custom: "x".repeat(CHOICE_TEXT_BYTES - 2) })).not.toThrow()
  expect(() => normalizeAnswer(q, { selected: [], custom: "x".repeat(CHOICE_TEXT_BYTES - 1) })).toThrow("8 KiB")
  const large = (allowCustom: boolean, count: number): Question => ({ prompt: [],  type: "choice", allowNotes: allowCustom, options: [{ id: "one", label: "First", value: "x".repeat(count) }] })
  expect(() => normalizeQuestion(large(false, ANSWER_BYTES))).toThrow("64 KiB")
  expect(() => normalizeQuestion(large(false, ANSWER_BYTES - 4096))).not.toThrow()
  expect(() => normalizeQuestion(large(true, ANSWER_BYTES - 4096))).toThrow("64 KiB")
  for (const control of [{ type: "text", options: [] }, { type: "unknown" }, { type: "choice", options: [] }, { type: "choice", options: [{ id: "a", label: "A", value: null }], allowCustom: null }])
    expect(() => normalizeQuestion({ prompt: [], ...control })).toThrow()
})

test("choice text size takes precedence over malformed envelopes and selections", () => {
 for (const shape of [{ selected: null }, {}, { selected: [], extra: true }, { selected: [{ id: "one", value: null }, { id: "two", value: false }] }]) {
  expect(() => normalizeAnswer(question(false, false), { ...shape, custom: "x".repeat(CHOICE_TEXT_BYTES - 1) })).toThrow("8 KiB")
 }
})
test("largest configured combined choice answer fits exactly 64 KiB", () => {
  const text = "x".repeat(CHOICE_TEXT_BYTES - 2)
  const fixed = new TextEncoder().encode(JSON.stringify({ selected: [{ id: "one", value: "" }], notes: text })).length
  const value = "x".repeat(ANSWER_BYTES - fixed)
  const q: Question = { prompt: [],  type: "choice", allowNotes: true, options: [{ id: "one", label: "One", value }] }
  expect(() => normalizeQuestion(q)).not.toThrow()
  expect(() => normalizeAnswer(q, { selected: [{ id: "one", value }], notes: text })).not.toThrow()
  expect(() => normalizeQuestion({ ...q,  type: "choice", allowNotes: true, options: [{ id: "one", label: "One", value: value + "x" }] })).toThrow("64 KiB")
})

test("canonical submissions carry IDs only and keep replay identity across order", () => {
  expect(normalizeSubmission({selectedIds:["two","one"],notes:"why"})).toEqual({selectedIds:["one","two"],notes:"why"})
  for (const value of [{selectedIds:["x","x"]},{selected:[]},{selectedIds:[],custom:" "},{selectedIds:[1]}]) expect(()=>normalizeSubmission(value)).toThrow()
})
test("single custom answers replace a selection while notes supplement it", () => {
  const q = {...question(false,true), allowNotes:true} as Question
  expect(()=>normalizeAnswer(q,{selected:[{id:"two",value:false}],custom:"other"})).toThrow()
  expect(normalizeAnswer(q,{selected:[],custom:"other",notes:"why"})).toEqual({selected:[],custom:"other",notes:"why"})
  expect(()=>normalizeAnswer(q,{selected:[],notes:"why"})).toThrow()
})


test("public question validation preserves shorthand, nested values and frozen inputs", () => {
  const input = Object.freeze({
    type: "choice", prompt: "Choose", title: "Pick",
    options: Object.freeze([Object.freeze({ id: "one", label: "First", value: Object.freeze({ exact: "雪" }) })]),
  })
  expect(validateQuestion(input)).toBeUndefined()
  expect(input.prompt).toBe("Choose")
  const unknown: unknown = { type: "text", prompt: "Explain" }
  validateQuestion(unknown)
  const typed: Question = unknown
  expect(typed.prompt).toBe("Explain")
})

test("public validation shares admission limits and structured errors", () => {
  const cases: unknown[] = [
    { type: "text", prompt: [], title: "雪".repeat(86) },
    { type: "text", prompt: [{ type: "file", uri: "private" }] },
    { type: "text", prompt: [], options: [] },
    { type: "choice", prompt: [], options: [{ id: "x", label: "X", value: null }, { id: "x", label: "Y", value: null }] },
    { type: "choice", prompt: [], options: [{ id: "x", label: "X", value: "x".repeat(ANSWER_BYTES) }] },
    { type: "text", prompt: "x".repeat(256 * 1024) },
    { type: "text", prompt: "\ud800" },
  ]
  for (const value of cases) {
    let admissionError: unknown
    try { normalizeQuestion(value) } catch (error) { admissionError = error }
    expect(admissionError).toBeInstanceOf(ContentError)
    try {
      validateQuestion(value)
      throw new Error("Expected validation failure")
    } catch (error) {
      expect(error).toBeInstanceOf(ContentError)
      expect((error as ContentError).code).toBe((admissionError as ContentError).code)
      expect((error as ContentError).message).toBe((admissionError as ContentError).message)
    }
  }
  expect(() => validateQuestion({ type: "text", prompt: [], title: "雪".repeat(85) })).not.toThrow()
})

test("question validation rejects accessors without executing them", () => {
  let reads = 0
  const input = { type: "text", get prompt() { reads++; return "Hidden" } }
  expect(() => validateQuestion(input)).toThrow(ContentError)
  expect(reads).toBe(0)
})


test("question validation rejects array accessors at every JSON nesting level", () => {
  for (const field of ["prompt", "options", "value"] as const) {
    let reads = 0
    const array: unknown[] = []
    Object.defineProperty(array, "0", { enumerable: true, get() { reads++; return null } })
    const input = field === "prompt" ? { type: "text", prompt: array }
      : field === "options" ? { type: "choice", prompt: [], options: array }
      : { type: "choice", prompt: [], options: [{ id: "a", label: "A", value: array }] }
    expect(() => validateQuestion(input)).toThrow(ContentError)
    expect(() => normalizeQuestion(input)).toThrow(ContentError)
    expect(reads).toBe(0)
  }
})
