import { expect, test } from "bun:test"
import type { Turn } from "@helmr/sdk"
import { answerQuestions } from "../agents/questions"

test("maps choices, multiple selections, custom text and text-only questions", async () => {
  const seen: any[] = []
  const replies = [{ selected: [{ value: "Blue" }, { value: "Green" }], custom: "Purple" }, "Longer answer"]
  const turn = { ask: async (question: unknown) => { seen.push(question); return { answer: replies.shift() } } } as unknown as Turn
  expect(await answerQuestions(turn, [
    { header: "Color", question: "Colors?", multiple: true, options: [{ label: "Blue", description: "blue" }, { label: "Green", description: "green" }] },
    { header: "Detail", question: "Details?", options: [] },
  ], new AbortController().signal)).toEqual([["Blue", "Green", "Purple"], ["Longer answer"]])
  expect(seen[0].multiple).toBe(true)
  expect(seen[0].allowCustom).toBe(true)
  expect(seen[1].type).toBe("text")
})

test("rejects an ambiguous batch before asking any question", async () => {
  let asked = false
  const turn = { ask: async () => { asked = true } } as unknown as Turn
  await expect(answerQuestions(turn, [{ header: "X", question: "X?", options: [{ label: "A", description: "one" }, { label: "A", description: "two" }] }], new AbortController().signal)).rejects.toThrow("unsupported question")
  expect(asked).toBe(false)
})


test("rejects a later invalid Helmr definition before displaying the first question", async () => {
  for (const invalid of [
    { header: "x".repeat(257), question: "Details?", options: [] },
    { header: "Color", question: "Color?", options: [{ label: "A", description: "x".repeat(2049) }] },
  ]) {
    let asked = 0
    const turn = { ask: async () => { asked++; return { answer: "answered" } } } as unknown as Turn
    await expect(answerQuestions(turn, [
      { header: "First", question: "First?", options: [] }, invalid,
    ], new AbortController().signal)).rejects.toThrow()
    expect(asked).toBe(0)
  }
})
