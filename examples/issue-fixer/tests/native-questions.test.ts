import { expect, test } from "bun:test"
import type { Turn } from "../../../sdk/typescript/src/agent"
import { normalizeAnswer, type Question } from "../../../sdk/typescript/src/question"
import { askNativeQuestions, nativeQuestions } from "../agents/issue-fixer/native-questions"
import { nativeText, steeringText } from "../agents/issue-fixer/native-content"

const question = (id: string) => ({ id, header: "Choice", question: "Choose", isOther: true, options: [{ label: "A", description: "First" }, { label: "B", description: "Second" }] })
test("native batch admits in source order and translates independent answers", async () => {
  const admitted: { question: Question; answer: (value: unknown) => void }[] = []
  const turn = { ask: (question: Question) => new Promise(resolve => admitted.push({ question, answer: value => resolve({ answer: normalizeAnswer(question, value), answeredBy: { kind: "user", id: "responder" } }) })) } as unknown as Turn
  const pending = askNativeQuestions(turn, nativeQuestions("codex", [question("one"), question("two")]), new AbortController().signal)
  expect(admitted.map(value => value.question.prompt)).toEqual([
    [{ type: "text", text: "1/2 Choose" }], [{ type: "text", text: "2/2 Choose" }],
  ])
  let done = false
  void pending.then(() => { done = true })
  admitted[1]!.answer({ selected: [{ id: "native-other", value: "None of the above" }], notes: "custom" })
  await Promise.resolve()
  expect(done).toBe(false)
  admitted[0]!.answer({ selected: [{ id: "option-2", value: "B" }] })
  expect((await pending).answers).toEqual({ one: ["B"], two: ["None of the above", "user_note: custom"] })
})
test("invalid later questions reject before any public ask is admitted", () => {
  for (const bad of [{ ...question("two"), isSecret: true }, { ...question("two"), preview: "private" }, question("one"), { ...question("two"), header: "x".repeat(257) }, { ...question("two"), options: [{ label: "A", description: "x".repeat(2049) }] }]) {
    expect(() => nativeQuestions("codex", [question("one"), bad])).toThrow()
  }
  expect(() => nativeQuestions("claude", [{ header: "h", question: "q", multiSelect: false, options: [{ label: "a", description: "a", preview: 123 }, { label: "b", description: "b" }] }])).toThrow()
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
test("content conversion preserves text and encodes JSON", () => {
  expect(nativeText([{ type: "text", text: "hel" }, { type: "text", text: "lo" }])).toBe("hello")
  expect(nativeText([{ type: "text", text: "inspect" }, { type: "json", value: { n: 3 } }])).toBe('inspect\n{"n":3}')
  expect(steeringText([{ type: "text", text: "stop editing" }])).toBe("stop editing")
})

test("Codex skip and notes stay distinct from None of the above and free text", async () => {
  const replies = [{ selected: [{ id: "native-skip", value: null }], notes: "Need more context" }, "Freeform"]
  const turn = { ask: async () => ({ answer: replies.shift() }) } as unknown as Turn
  const mapped = nativeQuestions("codex", [question("one"), { id: "free", header: "Details", question: "Explain" }])
  expect(mapped[0]!.question.title).toBe("Choice")
  expect(mapped[0]!.question.type === "choice" && mapped[0]!.question.allowCustom).toBe(false)
  expect((await askNativeQuestions(turn, mapped, new AbortController().signal)).answers).toEqual({ one: ["user_note: Need more context"], free: ["user_note: Freeform"] })
})

test("Claude previews and human notes are separate from label-based answers", async () => {
  const mapped = nativeQuestions("claude", [{ header: "Layout", question: "Which layout?", multiSelect: false, options: [{ label: "A", description: "First", preview: "<safe plain text>" }, { label: "B", description: "Second" }] }])
  expect(JSON.stringify(mapped[0]!.question.prompt)).toContain("<safe plain text>")
  const turn = { ask: async () => ({ answer: { selected: [{ id: "option-1", value: "A" }], notes: "Use this" } }) } as unknown as Turn
  expect(await askNativeQuestions(turn, mapped, new AbortController().signal)).toEqual({ answers: { "Which layout?": ["A"] }, annotations: { "Which layout?": { preview: "<safe plain text>", notes: "Use this" } } })
})
