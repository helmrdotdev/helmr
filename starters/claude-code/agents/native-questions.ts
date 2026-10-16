import { z } from "zod"
import type { JsonValue, Turn, Question, ChoiceAnswer } from "@helmr/sdk"
import { validateQuestion } from "@helmr/sdk"

const option = z.object({ label: z.string().min(1), description: z.string(), preview: z.string().optional() }).strict()
const claudeQuestion = z.object({
  header: z.string(), question: z.string().min(1), multiSelect: z.boolean(),
  options: z.array(option).min(2).max(4),
}).strict()
export const claudeQuestionInput = z.object({
  questions: z.array(claudeQuestion).min(1).max(4),
  answers: z.record(z.string(), z.string()).optional(),
  annotations: z.record(z.string(), z.object({ preview: z.string().optional(), notes: z.string().optional() }).strict()).optional(),
  metadata: z.object({ source: z.string().optional() }).strict().optional(),
}).strict()
interface NativeQuestion {
  key: string
  question: Question
  previews: Readonly<Record<string, string>>
}

// Validate the entire batch before any prompt can enter retained/public output.
export function nativeQuestions(input: unknown): NativeQuestion[] {
  const questions = z.array(claudeQuestion).min(1).max(4).parse(input)
  const keys = new Set<string>()
  return questions.map((source, index) => {
    const key = source.question
    if (keys.has(key)) throw new Error("Ambiguous native question identity")
    keys.add(key)
    if (new Set(source.options.map(value => value.label)).size !== source.options.length) throw new Error("Ambiguous native option labels")
    const previews: Record<string, string> = {}
    const previewText: string[] = []
    const options = source.options.map((value, ordinal) => {
      const id = `option-${ordinal + 1}`
      if (value.preview !== undefined) {
        previews[id] = value.preview
        previewText.push(`${value.label} — preview:\n${value.preview}`)
      }
      return { id, label: value.label, description: value.description, value: value.label as JsonValue }
    })
    const prompt = `${index + 1}/${questions.length} ${source.question}${previewText.length ? "\n\n" + previewText.join("\n\n") : ""}`
    const question: Question = {
      ...(source.header ? { title: source.header } : {}), prompt: [{ type: "text", text: prompt }],
      type: "choice", options, multiple: source.multiSelect, allowCustom: true, allowNotes: true,
    }
    validateQuestion(question)
    return { key, question, previews }
  })
}

export async function askNativeQuestions(turn: Turn, questions: NativeQuestion[], signal: AbortSignal) {
  signal.throwIfAborted()
  const siblings = new AbortController()
  const joined = AbortSignal.any([signal, siblings.signal])
  const jobs = questions.map(async ({ key, question, previews }) => {
    const { answer } = await turn.ask(question, { signal: joined })
    joined.throwIfAborted()
    const choice = answer as ChoiceAnswer
    const values = choice.selected.map(item => z.string().parse(item.value))
    if (choice.custom !== undefined) values.push(choice.custom)
    const preview = choice.selected.flatMap(item => previews[item.id] === undefined ? [] : [previews[item.id]]).join("\n\n")
    return { key, values, ...(preview || choice.notes !== undefined ? { annotation: { ...(preview ? { preview } : {}), ...(choice.notes === undefined ? {} : { notes: choice.notes }) } } : {}) }
  })
  try {
    const replies = await Promise.all(jobs)
    return {
      answers: Object.fromEntries(replies.map(reply => [reply.key, reply.values])),
      annotations: Object.fromEntries(replies.flatMap(reply => reply.annotation !== undefined ? [[reply.key, reply.annotation]] : [])),
    }
  } finally { siblings.abort(); await Promise.allSettled(jobs) }
}

export async function askNativeApproval(turn: Turn, action: JsonValue, signal: AbortSignal): Promise<boolean> {
  const { answer } = await turn.ask({
    prompt: [{ type: "text", text: "Allow this operation once?" }, { type: "json", value: action }],
    type: "choice", options: [
      { id: "allow", label: "Allow once", value: true },
      { id: "deny", label: "Deny", value: false },
    ],
  }, { signal })
  signal.throwIfAborted()
  return answer.selected.length === 1 && answer.selected[0]?.value === true
}
