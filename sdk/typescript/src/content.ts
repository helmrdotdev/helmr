import type { JsonValue } from "./contract"
import { canonicalizeJsonValue } from "./internal/jsoncanon"

export type ContentPart =
  | { readonly type: "text"; readonly text: string }
  | { readonly type: "json"; readonly value: JsonValue }
export type Content = readonly ContentPart[]
export type HumanContent = Content | string
export type InputPart = { readonly type: "text"; readonly text: string }
export type InputContent = readonly InputPart[]

export const CONTENT_BYTES = 256 * 1024
export const CONTENT_PARTS = 64
export const PROGRESS_BYTES = 8 * 1024 * 1024

export class ContentError extends Error {
  readonly code: "answer_invalid" | "invalid_arguments" | "content_kind_unsupported" | "content_limit_exceeded"
  constructor(code: ContentError["code"], message: string) { super(message); this.code = code; this.name = "ContentError" }
}

// Validate before cloning so accessors, invalid Unicode and non-JSON values are
// rejected rather than silently converted by JSON.stringify or structuredClone.
export function normalizeContent(value: unknown, shorthand = true): Content {
  const normalized = shorthand && typeof value === "string" ? [{ type: "text", text: value }] : value
  let encoded: Uint8Array
  try { encoded = canonicalizeJsonValue(normalized as JsonValue) }
  catch { throw new ContentError("invalid_arguments", "Content must be unambiguous finite JSON with valid Unicode") }
  if (encoded.byteLength > CONTENT_BYTES) throw new ContentError("content_limit_exceeded", "Content exceeds 256 KiB")
  if (!Array.isArray(normalized)) throw new ContentError("invalid_arguments", "Content must be an array")
  if (normalized.length > CONTENT_PARTS) throw new ContentError("content_limit_exceeded", "Content exceeds 64 parts")
  for (const part of normalized) {
    if (part === null || typeof part !== "object" || Array.isArray(part) || typeof part.type !== "string") throw new ContentError("invalid_arguments", "Invalid Content part")
    if (part.type !== "text" && part.type !== "json") throw new ContentError("content_kind_unsupported", "Content kind is unsupported")
    if (Object.keys(part).length !== 2 || (part.type === "text" ? typeof part.text !== "string" : !Object.hasOwn(part, "value"))) throw new ContentError("invalid_arguments", "Invalid Content part fields")
  }
  return JSON.parse(new TextDecoder().decode(encoded)) as Content
}

export function contentBytes(value: Content): number { return canonicalizeJsonValue(value).byteLength }

// Input is always a text-part array, including live messages and scheduled work.
export function normalizeInput(value: unknown): InputContent {
  const content = normalizeContent(value, false)
  if (content.some(part => part.type !== "text")) throw new ContentError("content_kind_unsupported", "Input supports text parts only")
  return content as InputContent
}
