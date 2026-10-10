import { expect, test } from "bun:test"
import type { Turn } from "../../../sdk/typescript/src/agent"
import { normalizeAnswer, type Question } from "../../../sdk/typescript/src/question"
import { askNativeQuestions, nativeQuestions } from "../tasks/issue-fixer/native-questions"
import { nativeText, steeringText } from "../tasks/issue-fixer/native-content"

const question = (id: string) => ({ id, header: "Choice", question: "Choose", isOther: true, options: [{ label: "A", description: "First" }, { label: "B", description: "Second" }] })
test("native batch admits in source order and translates independent answers", async () => {
  const admitted: { question: Question; answer: (value: unknown) => void }[] = []
  const turn = { ask: (question: Question) => new Promise(resolve => admitted.push({ question, answer: value => resolve({ answer: normalizeAnswer(question, value), respondedBy: { kind: "user", id: "responder" } }) })) } as unknown as Turn
  const pending = askNativeQuestions(turn, nativeQuestions("codex", [question("one"), question("two")]), new AbortController().signal)
  expect(admitted.map(value => value.question.prompt)).toEqual([
    [{ type: "text", text: "1/2 Choice\nChoose" }], [{ type: "text", text: "2/2 Choice\nChoose" }],
  ])
  let done = false
  void pending.then(() => { done = true })
  admitted[1]!.answer({ selected: [], text: "custom" })
  await Promise.resolve()
  expect(done).toBe(false)
  admitted[0]!.answer({ selected: [{ id: "option-2", value: "B" }] })
  expect(await pending).toEqual({ one: ["B"], two: ["custom"] })
})
test("invalid later questions reject before any public ask is admitted", () => {
  for (const bad of [{ ...question("two"), isSecret: true }, { ...question("two"), preview: "private" }, question("one")]) {
    expect(() => nativeQuestions("codex", [question("one"), bad])).toThrow()
  }
  expect(() => nativeQuestions("claude", [{ header: "h", question: "q", multiSelect: false, options: [{ label: "a", description: "a", preview: "rich" }, { label: "b", description: "b" }] }])).toThrow()
})
test("native cancellation withdraws unanswered siblings without assembling a reply", async () => {
  const stop = new AbortController()
  let withdrawn = 0
  const turn = { ask: (_: Question, { signal }: { signal: AbortSignal }) => new Promise((_, reject) => signal.addEventListener("abort", () => { withdrawn++; reject(signal.reason) }, { once: true })) } as unknown as Turn
  const pending = askNativeQuestions(turn, nativeQuestions("codex", [question("one"), question("two")]), stop.signal)
  stop.abort(new Error("native resolved"))
  await expect(pending).rejects.toThrow("native resolved")
  expect(withdrawn).toBe(2)
})
test("one failed ask withdraws its live siblings", async () => {
  let count = 0, withdrawn = false
  const turn = { ask: (_: Question, { signal }: { signal: AbortSignal }) => ++count === 1 ? Promise.reject(new Error("admission failed")) : new Promise((_, reject) => signal.addEventListener("abort", () => { withdrawn = true; reject(signal.reason) })) } as unknown as Turn
  await expect(askNativeQuestions(turn, nativeQuestions("codex", [question("one"), question("two")]), new AbortController().signal)).rejects.toThrow("admission failed")
  expect(withdrawn).toBe(true)
})
test("content conversion encodes JSON and rejects unsupported files", () => {
  expect(nativeText([{ type: "text", text: "hel" }, { type: "text", text: "lo" }])).toBe("hello")
  expect(nativeText([{ type: "text", text: "inspect" }, { type: "json", value: { n: 3 } }])).toBe('inspect\n{"n":3}')
  expect(steeringText([{ type: "text", text: "stop editing" }])).toBe("stop editing")
  expect(() => steeringText([{ type: "file", path: "/private" }])).toThrow()
})
