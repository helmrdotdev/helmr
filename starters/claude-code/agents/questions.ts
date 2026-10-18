import type { PermissionResult } from "@anthropic-ai/claude-agent-sdk"
import { validateQuestion, type JsonValue, type Question, type Turn } from "@helmr/sdk"
import { z } from "zod"

const questions = z.object({ questions: z.array(z.object({
  question: z.string().min(1), header: z.string(), multiSelect: z.boolean(),
  options: z.array(z.object({ label: z.string().min(1), description: z.string() })).min(1),
})).min(1) })

export async function askPermission(turn: Turn, name: string, input: Record<string, unknown>, title: string | undefined, signal: AbortSignal): Promise<PermissionResult> {
  signal.throwIfAborted()
  if (name === "AskUserQuestion") {
    const source = questions.parse(input).questions
    if (new Set(source.map(item => item.question)).size !== source.length) throw new Error("Duplicate Claude question")
    const mapped = source.map(item => {
      const question: Question = {
        title: item.header, prompt: [{ type: "text", text: item.question }], type: "choice",
        options: item.options.map((option, index) => ({ id: `option-${index}`, ...option, value: option.label as JsonValue })),
        multiple: item.multiSelect, allowCustom: true, allowNotes: true,
      }
      validateQuestion(question)
      return { source: item.question, question }
    })
    const siblings = new AbortController()
    const joined = AbortSignal.any([signal, siblings.signal])
    const pending = mapped.map(async ({ source, question }) => {
      const { answer } = await turn.ask(question, { signal: joined })
      joined.throwIfAborted()
      const values = answer.selected.map(option => z.string().parse(option.value))
      if (answer.custom) values.push(answer.custom)
      if (answer.notes) values.push(answer.notes)
      return [source, values.join(", ")] as const
    })
    try { return { behavior: "allow", updatedInput: { ...input, answers: Object.fromEntries(await Promise.all(pending)) } } }
    finally { siblings.abort(); await Promise.allSettled(pending) }
  }
  const { answer } = await turn.ask({
    prompt: [{ type: "text", text: title ?? `Allow ${name}?` }, { type: "json", value: JSON.parse(JSON.stringify(input)) }],
    type: "choice", options: [{ id: "allow", label: "Allow once", value: true }, { id: "deny", label: "Deny", value: false }],
  }, { signal })
  signal.throwIfAborted()
  return answer.selected.length === 1 && answer.selected[0]?.value === true
    ? { behavior: "allow", updatedInput: input }
    : { behavior: "deny", message: "The person denied this" }
}
