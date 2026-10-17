import type { ChoiceAnswer, JsonValue, Question, Turn } from "@helmr/sdk"
import { validateQuestion } from "@helmr/sdk"
import { z } from "zod"

const nativeQuestion = z.object({
  id: z.string().min(1), header: z.string(), question: z.string().min(1),
  isSecret: z.boolean().default(false), isOther: z.boolean().default(false),
  options: z.array(z.object({ label: z.string().min(1), description: z.string() })).nullable().optional(),
})

// Validate every question before publishing any of the batch to Slack/Console.
export async function askQuestions(turn: Turn, input: unknown, signal: AbortSignal) {
  const source = z.array(nativeQuestion).min(1).parse(input)
  if (new Set(source.map(item => item.id)).size !== source.length) throw new Error("Duplicate native question identity")
  const questions = source.map(item => {
    if (item.isSecret) throw new Error("Secret questions cannot be sent to a shared conversation")
    const options = item.options ?? []
    if (new Set(options.map(option => option.label)).size !== options.length) throw new Error("Duplicate native option label")
    const choices = options.map((option, index) => ({ id: `option-${index}`, ...option, value: option.label as JsonValue }))
    if (choices.length) {
      if (item.isOther) choices.push({ id: "other", label: "None of the above", description: "Add details in notes.", value: "None of the above" })
      choices.push({ id: "skip", label: "Skip this question", description: "Continue without an answer.", value: null })
    }
    const question: Question = {
      ...(item.header ? { title: item.header } : {}), prompt: [{ type: "text", text: item.question }],
      ...(choices.length ? { type: "choice", options: choices, multiple: false, allowCustom: false, allowNotes: true } : { type: "text" }),
    }
    validateQuestion(question)
    return { id: item.id, question }
  })
  signal.throwIfAborted()
  const siblings = new AbortController()
  const joined = AbortSignal.any([signal, siblings.signal])
  const pending = questions.map(async ({ id, question }) => {
    const { answer } = await turn.ask(question, { signal: joined })
    joined.throwIfAborted()
    if (question.type === "text") {
      const text = z.string().parse(answer).trim()
      return [id, { answers: text ? [`user_note: ${text}`] : [] }] as const
    }
    const choice = answer as ChoiceAnswer
    const answers = choice.selected.filter(option => option.id !== "skip").map(option => z.string().parse(option.value))
    if (choice.notes !== undefined) answers.push(`user_note: ${choice.notes}`)
    return [id, { answers }] as const
  })
  try { return { answers: Object.fromEntries(await Promise.all(pending)) } }
  finally { siblings.abort(); await Promise.allSettled(pending) }
}

export async function askApproval(turn: Turn, action: JsonValue, signal: AbortSignal, denyDecision: "decline" | "cancel") {
  signal.throwIfAborted()
  const { answer } = await turn.ask({
    prompt: [{ type: "text", text: "Allow this operation once?" }, { type: "json", value: action }],
    type: "choice", options: [
      { id: "allow", label: "Allow once", value: true },
      { id: "deny", label: "Deny", value: false },
    ],
  }, { signal })
  signal.throwIfAborted()
  return { decision: answer.selected.length === 1 && answer.selected[0]?.value === true ? "accept" : denyDecision }
}
