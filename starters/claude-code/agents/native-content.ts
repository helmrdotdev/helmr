import type { HumanContent, InputContent } from "@helmr/sdk"

// JSON remains visibly encoded; portable content never becomes a local file path.
export function nativeText(content: HumanContent): string {
  if (typeof content === "string") return content
  let text = "", previous: "text" | "json" | undefined
  for (const part of content) {
    if (previous !== undefined && (previous === "json" || part.type === "json")) text += "\n"
    text += part.type === "text" ? part.text : JSON.stringify(part.value)
    previous = part.type
  }
  return text
}
export function steeringText(value: InputContent): string {
  return value.map(part => part.text).join("")
}
