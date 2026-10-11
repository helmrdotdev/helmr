import { z } from "zod"
import type { Json, Turn } from "@helmr/sdk"
import { normalizeQuestion } from "@helmr/sdk/internal"
import type { Question, ChoiceAnswer } from "@helmr/sdk"

const option = z.object({ label: z.string().min(1), description: z.string() }).strict()
const codexQuestion = z.object({
  id: z.string().min(1), header: z.string(), question: z.string(),
  isSecret: z.boolean().default(false), isOther: z.boolean().default(false),
  options: z.array(option).nullable().optional(),
}).strict()
const claudeQuestion = z.object({
  header: z.string(), question: z.string().min(1), multiSelect: z.boolean(),
  options: z.array(option).min(2).max(4),
}).strict()
interface NativeQuestion { key: string; question: Question }

// Validate the entire batch before any prompt can enter retained/public output.
export function nativeQuestions(provider: "codex" | "claude", input: unknown): NativeQuestion[] {
  const questions = provider === "codex"
    ? z.array(codexQuestion).min(1).parse(input)
    : z.array(claudeQuestion).min(1).max(4).parse(input)
  const keys = new Set<string>()
  return questions.map((source, index) => {
    if ("isSecret" in source && source.isSecret) throw new Error("Secret native questions require a private channel")
    const key = "id" in source ? source.id : source.question
    if (keys.has(key)) throw new Error("Ambiguous native question identity")
    keys.add(key)
    const options = source.options
    if (options && new Set(options.map(value => value.label)).size !== options.length) throw new Error("Ambiguous native option labels")
    const question = normalizeQuestion({
      prompt: [{ type: "text", text: `${index + 1}/${questions.length} ${source.header}\n${source.question}` }],
      answer: options?.length ? {
        type: "choice", options: options.map((value, ordinal) => ({ id: `option-${ordinal + 1}`, label: value.label, description: value.description, value: value.label })),
        multiple: "multiSelect" in source ? source.multiSelect : false,
        allowText: "isOther" in source ? source.isOther : true,
      } : { type: "text" },
    })
    return { key, question }
  })
}

export async function askNativeQuestions(turn: Turn, questions: NativeQuestion[], signal: AbortSignal): Promise<Record<string, string[]>> {
  signal.throwIfAborted()
  const siblings = new AbortController()
  const joined = AbortSignal.any([signal, siblings.signal])
  const jobs = questions.map(async ({ key, question }) => {
    const { answer } = await turn.ask(question, { signal: joined })
    joined.throwIfAborted()
    if (question.answer.type === "text") return [key, [z.string().parse(answer)]] as const
    const choice = answer as unknown as ChoiceAnswer
    const values = choice.selected.map(value => z.string().parse(value.value))
    if (choice.text !== undefined && choice.text !== "") values.push(choice.text)
    return [key, values] as const
  })
  try { return Object.fromEntries(await Promise.all(jobs)) }
  finally { siblings.abort(); await Promise.allSettled(jobs) }
}

export async function askNativeApproval(turn: Turn, action: Json, signal: AbortSignal): Promise<boolean> {
  const { answer } = await turn.ask({
    prompt: [{ type: "text", text: "Allow this operation once?" }, { type: "json", value: action }],
    answer: { type: "choice", options: [
      { id: "allow", label: "Allow once", value: true },
      { id: "deny", label: "Deny", value: false },
    ] },
  }, { signal })
  signal.throwIfAborted()
  return answer.selected.length === 1 && answer.selected[0]?.value === true
}
