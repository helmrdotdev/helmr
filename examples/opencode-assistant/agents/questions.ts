import { validateQuestion, type Question, type Turn } from "@helmr/sdk"
import type { Form } from "@opencode/schema/form"
import type { Permission } from "@opencode/schema/permission"

// This example grants each operation once; it never saves native permissions.
export async function askPermission(turn: Turn, request: Permission.Request, signal: AbortSignal): Promise<Permission.Reply> {
  signal.throwIfAborted()
  const options = [
    { id: "once", label: "Allow once", value: "once" },
    { id: "reject", label: "Deny", value: "reject" },
  ]
  const { answer } = await turn.ask({ type: "choice", prompt: [{ type: "text", text: request.message ?? `Allow ${request.action}?` }, { type: "json", value: [...request.resources] }], options }, { signal })
  signal.throwIfAborted()
  const selected = answer.selected[0]?.value
  return answer.selected.length === 1 && selected === "once" ? selected : "reject"
}

export async function askForm(turn: Turn, form: Form.Info, signal: AbortSignal): Promise<Form.Answer> {
  const answer: Record<string, string | number | boolean | readonly string[]> = {}
  for (const field of form.fields) {
    if (field.type === "external") throw new Error("OpenCode external forms require the native interface")
    if (field.when?.some(condition => condition.op === "eq" ? answer[condition.key] !== condition.value : answer[condition.key] === condition.value)) continue
    if (field.hidden) { if (field.default !== undefined) answer[field.key] = field.default; continue }
    const prompt = [{ type: "text" as const, text: field.description ?? field.title ?? field.key }]
    let question: Question
    if (field.type === "boolean") question = { type: "choice", prompt, options: [{ id: "yes", label: "Yes", value: true }, { id: "no", label: "No", value: false }] }
    else if (field.type === "multiselect" || (field.type === "string" && field.options?.length)) {
      question = { type: "choice", prompt,
        options: (field.options ?? []).map((option, index) => ({ id: `option-${index}`, label: option.label, description: option.description, value: option.value })),
        multiple: field.type === "multiselect", allowCustom: field.type === "string" && field.custom,
      }
    } else question = { type: "text", prompt }
    validateQuestion(question)
    signal.throwIfAborted()
    const result = await turn.ask(question, { signal })
    signal.throwIfAborted()
    if (question.type === "text") {
      const text = result.answer as string
      if (!text.trim()) {
        if (field.required) throw new Error("OpenCode form requires an answer")
        continue
      }
      if (field.type === "number" || field.type === "integer") {
        const number = Number(text)
        if (!Number.isFinite(number) || (field.type === "integer" && !Number.isInteger(number))) throw new Error("OpenCode form requires a valid number")
        answer[field.key] = number
      } else answer[field.key] = text
    } else {
      const choice = result.answer as import("@helmr/sdk").ChoiceAnswer
      if (field.type === "multiselect") answer[field.key] = choice.selected.map(item => String(item.value))
      else if (choice.custom) answer[field.key] = choice.custom
      else if (choice.selected.length === 1) answer[field.key] = choice.selected[0]!.value as string | boolean
    }
  }
  return answer
}
