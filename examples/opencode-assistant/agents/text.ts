import type { Event, TextPart } from "@opencode-ai/sdk/v2"

// Only completed, user-facing text from this prompt's assistant messages is
// eligible for output. Snapshots replace parts; deltas never append a second copy.
export class OpenCodeText {
  private readonly messages = new Map<string, boolean>()
  private readonly parts = new Map<string, TextPart>()
  private readonly published = new Set<string>()
  private current: string | undefined
  private bytes = 0
  constructor(private readonly sessionID: string, readonly promptID: string, private readonly output: (text: string) => void) {}
  observe(event: Event): void {
    if (!("sessionID" in event.properties) || event.properties.sessionID !== this.sessionID) return
    if (event.type === "message.updated") {
      const info = event.properties.info
      if (info.sessionID !== this.sessionID || info.role !== "assistant" || info.parentID !== this.promptID || info.summary) return
      if (this.messages.size >= 16384 && !this.messages.has(info.id)) throw new Error("OpenCode message limit exceeded")
      if (this.current !== undefined && this.current !== info.id) this.flush(this.current)
      this.current = info.id
      this.messages.set(info.id, info.time.completed !== undefined)
      if (info.finish === "tool-calls") this.flush(info.id)
      return
    }
    if (event.type !== "message.part.updated") return
    const part = event.properties.part
    if (part.sessionID !== this.sessionID || !this.messages.has(part.messageID)) return
    if (part.type === "tool") { this.flush(part.messageID); return }
    if (part.type !== "text" || part.synthetic || part.ignored) return
    if (this.published.has(part.id)) return
    const previous = this.parts.get(part.id)
    const bytes = this.bytes - (previous ? Buffer.byteLength(previous.text) : 0) + Buffer.byteLength(part.text)
    if (bytes > 256 * 1024 || (this.parts.size + this.published.size >= 16384 && previous === undefined)) throw new Error("OpenCode text buffer limit exceeded")
    this.bytes = bytes
    this.parts.set(part.id, part)
  }
  flush(messageID?: string): void {
    for (const [id, part] of this.parts) {
      if (messageID !== undefined && part.messageID !== messageID) continue
      if (part.time?.end === undefined && this.messages.get(part.messageID) !== true) continue
      this.output(part.text)
      this.bytes -= Buffer.byteLength(part.text)
      this.parts.delete(id)
      this.published.add(id)
    }
  }
  finish(finalMessageID: string): void {
    for (const id of this.messages.keys()) if (id !== finalMessageID) this.flush(id)
  }
}
