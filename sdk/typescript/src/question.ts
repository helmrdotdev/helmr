import type { JsonValue } from "./contract"
import { CONTENT_BYTES, ContentError, normalizeContent, type Content } from "./content"
import { canonicalizeJsonValue } from "./internal/jsoncanon"

export type ChoiceOption = Readonly<{ id: string; label: string; description?: string; value: JsonValue }>
export type AnswerControl =
  | Readonly<{ type: "text" }>
  | Readonly<{ type: "choice"; options: readonly ChoiceOption[]; multiple?: boolean; allowText?: boolean }>
export type Question<C extends AnswerControl = AnswerControl> = Readonly<{ prompt: Content; answer: C }>
export type ChoiceAnswer = Readonly<{ selected: readonly Readonly<{ id: string; value: JsonValue }>[]; text?: string }>
export type AnswerFor<C extends AnswerControl> = C extends { type: "text" } ? string : ChoiceAnswer
export type AskResponse<T = JsonValue> = Readonly<{ answer: T; respondedBy: Readonly<{ kind: "user" | "api_key"; id: string }> }>
export const ANSWER_BYTES = 64 * 1024
export const CHOICE_TEXT_BYTES = 8 * 1024
const encoder = new TextEncoder()

function invalid(message = "Invalid question/control definition"): never { throw new ContentError("invalid_arguments", message) }
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

export function normalizeQuestion(value: unknown): Question {
  const bytes = canonical(value)
  if (bytes.byteLength > CONTENT_BYTES) return limit("Question exceeds 256 KiB")
  const copy: unknown = JSON.parse(new TextDecoder().decode(bytes))
  const question = fields(copy, ["prompt", "answer"])
  normalizeContent(question["prompt"], false)
  const control = fields(question["answer"], ["type"], ["options", "multiple", "allowText"])
  if (control["type"] === "text") {
    if (Object.keys(control).length !== 1) return invalid()
  } else {
    if (control["type"] !== "choice" || !Array.isArray(control["options"]) || control["options"].length === 0) return invalid()
    if (control["options"].length > 100) return limit("Choice exceeds 100 options")
    for (const key of ["multiple", "allowText"]) if (Object.hasOwn(control, key) && typeof control[key] !== "boolean") return invalid()
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
    const maximum = Math.max(...selections.map((selected) => canonical({ selected }).byteLength)) + (control["allowText"] ? 8 + CHOICE_TEXT_BYTES : 0)
    if (maximum > ANSWER_BYTES) return limit("Configured choice answer exceeds 64 KiB")
  }
  return copy as Question
}

export function normalizeAnswer(question: Question, value: unknown): JsonValue {
  let bytes: Uint8Array
  try { bytes = canonicalizeJsonValue(value as JsonValue) }
  catch { throw new ContentError("answer_invalid", "Answer must be finite JSON with valid Unicode") }
  if (bytes.byteLength > ANSWER_BYTES) return limit("Answer exceeds 64 KiB")
  const copy: JsonValue = JSON.parse(new TextDecoder().decode(bytes))
  const control = normalizeQuestion(question).answer
  const rejected = (): never => { throw new ContentError("answer_invalid", "Answer does not match its declared control") }
  if (control.type === "text") return typeof copy === "string" ? copy : rejected()
  // A present custom-text size violation precedes choice shape rejection.
  if (copy !== null && typeof copy === "object" && !Array.isArray(copy) && Object.hasOwn(copy, "text") && canonical((copy as Record<string, JsonValue>)["text"]).byteLength > CHOICE_TEXT_BYTES) return limit("Choice custom text exceeds 8 KiB")
  let answer: Record<string, unknown>
  try { answer = fields(copy, ["selected"], ["text"]) } catch { return rejected() }
  const selected = answer["selected"]
  if (!Array.isArray(selected) || (!control.multiple && selected.length > 1)) return rejected()
  let text = ""
  if (Object.hasOwn(answer, "text")) {
    if (!control.allowText || typeof answer["text"] !== "string") return rejected()
    text = answer["text"]
  }
  if (selected.length === 0 && text === "") return rejected()
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
