import { compareUTF8 } from "./internal/utf8"
import type { JsonValue } from "./contract"
import { CONTENT_BYTES, ContentError, normalizeContent, type Content, type HumanContent } from "./content"
import { canonicalizeJsonValue } from "./internal/jsoncanon"

export type ChoiceOption = Readonly<{ id: string; label: string; description?: string; value: JsonValue }>
type QuestionBase = Readonly<{ prompt: HumanContent; title?: string }>
export type TextQuestion = QuestionBase & Readonly<{ type: "text"; options?: never; multiple?: never; allowCustom?: never; allowNotes?: never }>
export type ChoiceQuestion = QuestionBase & Readonly<{ type: "choice"; options: readonly ChoiceOption[]; multiple?: boolean; allowCustom?: boolean; allowNotes?: boolean }>
export type Question = TextQuestion | ChoiceQuestion
export type NormalizedQuestion = Question & Readonly<{ prompt: Content }>
type Selection<O> = O extends ChoiceOption ? Readonly<{ id: O["id"]; value: O["value"] }> : never
export type ChoiceAnswer<Q extends ChoiceQuestion = ChoiceQuestion> = Readonly<{ selected: readonly Selection<Q["options"][number]>[] }>
  & (true extends ("allowCustom" extends keyof Q ? Q["allowCustom"] : false) ? Readonly<{ custom?: string }> : Readonly<{ custom?: never }>)
  & (true extends ("allowNotes" extends keyof Q ? Q["allowNotes"] : false) ? Readonly<{ notes?: string }> : Readonly<{ notes?: never }>)
export type AnswerFor<Q extends Question> = Q extends TextQuestion ? string : Q extends ChoiceQuestion ? ChoiceAnswer<Q> : never
export type AskAnswer<T = JsonValue> = Readonly<{ answer: T; answeredBy: Readonly<{ kind: "user" | "api_key"; id: string }> }>
export type AnswerSubmission = string | Readonly<{ selectedIds: readonly string[]; custom?: string; notes?: string }>
export const ANSWER_BYTES = 64 * 1024
export const CHOICE_TEXT_BYTES = 8 * 1024
const encoder = new TextEncoder()

function invalid(message = "Invalid question definition"): never { throw new ContentError("invalid_arguments", message) }
function limit(message: string): never { throw new ContentError("content_limit_exceeded", message) }
function fields(value: unknown, required: readonly string[], optional: readonly string[] = []): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return invalid()
  const record = value as Record<string, unknown>
  if (required.some((key) => !Object.hasOwn(record, key)) || Object.keys(record).some((key) => !required.includes(key) && !optional.includes(key))) return invalid()
  return record
}
function canonical(value: unknown): Uint8Array {
  try { return canonicalizeJsonValue(value as JsonValue) } catch { return invalid("Question must be unambiguous finite JSON with valid Unicode") }
}
function byteLength(text: string): number { return encoder.encode(text).byteLength }

export function normalizeQuestion(value: unknown): NormalizedQuestion {
  const bytes = canonical(value)
  if (bytes.byteLength > CONTENT_BYTES) return limit("Question exceeds 256 KiB")
  const copy: unknown = JSON.parse(new TextDecoder().decode(bytes))
  const question = fields(copy, ["prompt", "type"], ["title", "options", "multiple", "allowCustom", "allowNotes"])
  question["prompt"] = normalizeContent(question["prompt"])
  if (Object.hasOwn(question, "title") && (typeof question["title"] !== "string" || !question["title"].trim() || byteLength(question["title"]) > 256)) return invalid("Invalid question title")
  if (canonical(question).byteLength > CONTENT_BYTES) return limit("Question exceeds 256 KiB")
  const control = question
  if (control["type"] === "text") {
    if (["options", "multiple", "allowCustom", "allowNotes"].some(key => Object.hasOwn(control, key))) return invalid()
  } else {
    if (control["type"] !== "choice" || !Array.isArray(control["options"]) || control["options"].length === 0) return invalid()
    if (control["options"].length > 100) return limit("Choice exceeds 100 options")
    for (const key of ["multiple", "allowCustom", "allowNotes"]) if (Object.hasOwn(control, key) && typeof control[key] !== "boolean") return invalid()
    const ids = new Set<string>()
    const options = control["options"].map((value) => {
      const option = fields(value, ["id", "label", "value"], ["description"])
      const id = option["id"], label = option["label"], description = option["description"]
      if (typeof id !== "string" || !id || ids.has(id) || typeof label !== "string") return invalid()
      if (byteLength(id) > 128 || byteLength(label) > 256) return limit("Choice ID exceeds 128 bytes or label exceeds 256 bytes")
      if (Object.hasOwn(option, "description")) {
        if (typeof description !== "string") return invalid()
        if (byteLength(description) > 2048) return limit("Choice description exceeds 2 KiB")
      }
      ids.add(id)
      return { id, value: option["value"] as JsonValue }
    })
    const selections = control["multiple"] ? [options] : options.map((option) => [option])
    let maximum = Math.max(...selections.map((selected) => canonical({ selected }).byteLength))
    if (control["allowCustom"]) maximum = control["multiple"] ? maximum + 10 + CHOICE_TEXT_BYTES : Math.max(maximum, canonical({ selected: [] }).byteLength + 10 + CHOICE_TEXT_BYTES)
    if (control["allowNotes"]) maximum += 9 + CHOICE_TEXT_BYTES
    if (maximum > ANSWER_BYTES) return limit("Configured choice answer exceeds 64 KiB")
  }
  return copy as NormalizedQuestion
}

export function normalizeAnswer(question: Question, value: unknown): JsonValue {
  let bytes: Uint8Array
  try { bytes = canonicalizeJsonValue(value as JsonValue) }
  catch { throw new ContentError("answer_invalid", "Answer must be finite JSON with valid Unicode") }
  if (bytes.byteLength > ANSWER_BYTES) return limit("Answer exceeds 64 KiB")
  const copy: JsonValue = JSON.parse(new TextDecoder().decode(bytes))
  const control = normalizeQuestion(question)
  const rejected = (): never => { throw new ContentError("answer_invalid", "Answer does not match its declared control") }
  if (control.type === "text") return typeof copy === "string" ? copy : rejected()
  if (copy !== null && typeof copy === "object" && !Array.isArray(copy)) {
    for (const key of ["custom", "notes"]) if (Object.hasOwn(copy, key) && canonical((copy as Record<string, JsonValue>)[key]).byteLength > CHOICE_TEXT_BYTES) return limit("Choice text exceeds 8 KiB")
  }
  let answer: Record<string, unknown>
  try { answer = fields(copy, ["selected"], ["custom", "notes"]) } catch { return rejected() }
  const selected = answer["selected"]
  if (!Array.isArray(selected) || (!control.multiple && selected.length > 1)) return rejected()
  for (const key of ["custom", "notes"] as const) {
    if (Object.hasOwn(answer, key) && (!(key === "custom" ? control.allowCustom : control.allowNotes) || typeof answer[key] !== "string" || !answer[key].trim())) return rejected()
  }
  if (selected.length === 0 && !answer["custom"]) return rejected()
  if (!control.multiple && selected.length > 0 && answer["custom"]) return rejected()
  let previous = -1
  for (const item of selected) {
    let choice: Record<string, unknown>
    try { choice = fields(item, ["id", "value"]) } catch { return rejected() }
    const index = control.options.findIndex((option) => option.id === choice["id"])
    if (index <= previous || index < 0) return rejected()
    if (new TextDecoder().decode(canonical(choice["value"])) !== new TextDecoder().decode(canonical(control.options[index]!.value))) return rejected()
    previous = index
  }
  return copy
}

/** Canonical replay identity, independent of retained question payload. */
export function normalizeSubmission(value: unknown): AnswerSubmission {
  const rejected = (): never => { throw new ContentError("answer_invalid", "Invalid answer submission") }
  let bytes: Uint8Array
  try { bytes = canonicalizeJsonValue(value as JsonValue) } catch { return rejected() }
  if (bytes.byteLength > ANSWER_BYTES) return limit("Answer exceeds 64 KiB")
  const copy: unknown = JSON.parse(new TextDecoder().decode(bytes))
  if (typeof copy === "string") return copy
  if (copy !== null && typeof copy === "object" && !Array.isArray(copy)) for (const key of ["custom", "notes"]) {
    if (Object.hasOwn(copy, key) && canonical((copy as Record<string, unknown>)[key]).byteLength > CHOICE_TEXT_BYTES) return limit("Choice text exceeds 8 KiB")
  }
  let answer: Record<string, unknown>
  try { answer = fields(copy, ["selectedIds"], ["custom", "notes"]) } catch { return rejected() }
  if (!Array.isArray(answer["selectedIds"])) return rejected()
  const ids: string[] = []
  for (const id of answer["selectedIds"]) {
    if (typeof id !== "string" || !id || byteLength(id) > 128 || ids.includes(id)) return rejected()
    ids.push(id)
  }
  ids.sort(compareUTF8)
  for (const key of ["custom", "notes"]) {
    if (!Object.hasOwn(answer, key)) continue
    if (canonical(answer[key]).byteLength > CHOICE_TEXT_BYTES) return limit("Choice text exceeds 8 KiB")
    if (typeof answer[key] !== "string" || !answer[key].trim()) return rejected()
  }
  return { selectedIds: ids, ...(typeof answer["custom"] === "string" ? { custom: answer["custom"] } : {}), ...(typeof answer["notes"] === "string" ? { notes: answer["notes"] } : {}) }
}
