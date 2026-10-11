import { normalizeContent, normalizeInput } from "@helmr/sdk/internal"
import type { HumanContent } from "@helmr/sdk"

// JSON remains visibly encoded; portable content never becomes a local file path.
export function nativeText(content: HumanContent): string {
  let text = "", previous: "text" | "json" | undefined
  for (const part of normalizeContent(content)) {
    if (previous !== undefined && (previous === "json" || part.type === "json")) text += "\n"
    text += part.type === "text" ? part.text : JSON.stringify(part.value)
    previous = part.type
  }
  return text
}
export function steeringText(value: unknown): string {
  return normalizeInput(value).map(part => part.text).join("")
}
