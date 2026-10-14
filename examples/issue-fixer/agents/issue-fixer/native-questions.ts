import { z } from "zod"
import type { Json, Turn, Question, ChoiceAnswer } from "@helmr/sdk"
import { validateQuestion } from "@helmr/sdk"

const option = z.object({ label: z.string().min(1), description: z.string() }).strict()
const codexQuestion = z.object({
  id: z.string().min(1), header: z.string(), question: z.string(),
  isSecret: z.boolean().default(false), isOther: z.boolean().default(false),
  options: z.array(option).nullable().optional(),
}).strict()
const claudeQuestion = z.object({
  header: z.string(), question: z.string().min(1), multiSelect: z.boolean(),
  options: z.array(option.extend({ preview: z.string().optional() })).min(2).max(4),
}).strict()
export const claudeQuestionInput = z.object({
  questions: z.array(claudeQuestion).min(1).max(4),
  answers: z.record(z.string(), z.string()).optional(),
  annotations: z.record(z.string(), z.object({ preview: z.string().optional(), notes: z.string().optional() }).strict()).optional(),
  metadata: z.object({ source: z.string().optional() }).strict().optional(),
}).strict()
interface NativeQuestion {
  key: string
  provider: "codex" | "claude"
  question: Question
  previews: Readonly<Record<string, string>>
}

// Validate the entire batch before any prompt can enter retained/public output.
export function nativeQuestions(provider: "codex" | "claude", input: unknown): NativeQuestion[] {
  const questions = provider === "codex" ? z.array(codexQuestion).min(1).parse(input) : z.array(claudeQuestion).min(1).max(4).parse(input)
  const keys = new Set<string>()
  return questions.map((source, index) => {
    if ("isSecret" in source && source.isSecret) throw new Error("Secret native questions require a private channel")
    const key = "id" in source ? source.id : source.question
    if (keys.has(key)) throw new Error("Ambiguous native question identity")
    keys.add(key)
    const options = source.options
    if (options && new Set(options.map(value => value.label)).size !== options.length) throw new Error("Ambiguous native option labels")
    const previews: Record<string, string> = {}
    const previewText: string[] = []
    const choices = (options ?? []).map((value, ordinal) => {
      const id = `option-${ordinal + 1}`
      if ("preview" in value && typeof value.preview === "string") {
        previews[id] = value.preview
        previewText.push(`${value.label} — preview:\n${value.preview}`)
      }
      return { id, label: value.label, description: value.description, value: value.label as Json }
    })
    if (provider === "codex" && choices.length) {
      if ("isOther" in source && source.isOther) choices.push({ id: "native-other", label: "None of the above", description: "Optionally add details in notes.", value: "None of the above" })
      choices.push({ id: "native-skip", label: "Skip this question", description: "Continue without selecting an answer to this question.", value: null })
    }
    const prompt = `${index + 1}/${questions.length} ${source.question}${previewText.length ? "\n\n" + previewText.join("\n\n") : ""}`
    const question: Question = {
      ...(source.header ? { title: source.header } : {}), prompt: [{ type: "text", text: prompt }],
      ...(choices.length ? {
        type: "choice", options: choices,
        multiple: "multiSelect" in source ? source.multiSelect : false,
        allowCustom: provider === "claude", allowNotes: true,
      } : { type: "text" }),
    }
    validateQuestion(question)
    return { key, provider, question, previews }
  })
}

export async function askNativeQuestions(turn: Turn, questions: NativeQuestion[], signal: AbortSignal) {
  signal.throwIfAborted()
  const siblings = new AbortController()
  const joined = AbortSignal.any([signal, siblings.signal])
  const jobs = questions.map(async ({ key, provider, question, previews }) => {
    const { answer } = await turn.ask(question, { signal: joined })
    joined.throwIfAborted()
    if (question.type === "text") {
      const text = z.string().parse(answer)
      return { key, values: provider === "codex" ? (text.trim() ? [`user_note: ${text.trim()}`] : []) : [text] }
    }
    const choice = answer as ChoiceAnswer
    const values = choice.selected.filter(item => item.id !== "native-skip").map(item => z.string().parse(item.value))
    if (choice.custom !== undefined) values.push(choice.custom)
    if (provider === "codex" && choice.notes !== undefined) values.push(`user_note: ${choice.notes}`)
    const preview = choice.selected.flatMap(item => previews[item.id] === undefined ? [] : [previews[item.id]]).join("\n\n")
    return { key, values, ...(provider === "claude" && (preview || choice.notes !== undefined) ? { annotation: { ...(preview ? { preview } : {}), ...(choice.notes === undefined ? {} : { notes: choice.notes }) } } : {}) }
  })
  try {
    const replies = await Promise.all(jobs)
    return {
      answers: Object.fromEntries(replies.map(reply => [reply.key, reply.values])),
      annotations: Object.fromEntries(replies.flatMap(reply => "annotation" in reply && reply.annotation !== undefined ? [[reply.key, reply.annotation]] : [])),
    }
  } finally { siblings.abort(); await Promise.allSettled(jobs) }
}

export async function askNativeApproval(turn: Turn, action: Json, signal: AbortSignal): Promise<boolean> {
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
