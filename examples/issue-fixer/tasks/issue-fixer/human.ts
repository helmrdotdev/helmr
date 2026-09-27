import { createHash, randomUUID } from "node:crypto"
import { MessageRejected, type JsonValue } from "@helmr/sdk"
import { z } from "zod"

// These are application messages, not additions to Helmr's protocol.
export const replySchema = z.discriminatedUnion("type", [
  z.object({ type: z.literal("approval"), requestId: z.string(), allow: z.boolean() }),
  z.object({ type: z.literal("answer"), requestId: z.string(), answers: z.record(z.string(), z.array(z.string().min(1)).min(1)) }),
  z.object({ type: z.literal("update_constraints"), text: z.string().min(1) }),
])
export type Reply = z.infer<typeof replySchema>
type HumanReply = Exclude<Reply, { type: "update_constraints" }>
type Pending = {
  kind: "approval" | "answer"
  actionBinding: string
  keys?: string[]
  signal: AbortSignal
  claimed: boolean
  resolve: (reply: HumanReply) => void
  reject: (error: Error) => void
}

// A live, per-native-operation inbox. It is deliberately not a durable wait.
export class HumanRequests {
  private readonly pending = new Map<string, Pending>()
  constructor(private readonly write: (value: JsonValue) => Promise<unknown>, private readonly signal: AbortSignal) {}

  async ask(kind: Pending["kind"], action: JsonValue, nativeSignal: AbortSignal, keys?: string[]): Promise<HumanReply> {
    const signal = AbortSignal.any([this.signal, nativeSignal])
    signal.throwIfAborted()
    const requestId = randomUUID() // Unique even if a provider reuses its native request ID.
    const actionBinding = createHash("sha256").update(JSON.stringify(action)).digest("hex")
    let cancel!: () => void
    const response = new Promise<HumanReply>((resolve, reject) => {
      this.pending.set(requestId, { kind, actionBinding, keys, signal, claimed: false, resolve, reject })
      cancel = () => reject(new Error("Native request is no longer live"))
      signal.addEventListener("abort", cancel, { once: true })
    })
    // Attach a rejection handler before publication, which may itself be delayed.
    void response.catch(() => {})
    try {
      await this.write({ type: "human_requested", requestId, kind, actionBinding, action })
      const reply = await response
      signal.throwIfAborted()
      return reply
    } finally {
      this.pending.delete(requestId)
      signal.removeEventListener("abort", cancel)
    }
  }

  async reply(reply: HumanReply): Promise<void> {
    const request = this.pending.get(reply.requestId)
    if (!request || request.claimed || request.signal.aborted || request.kind !== reply.type) {
      throw new MessageRejected("Request is stale, already answered, or has a different reply kind")
    }
    if (reply.type === "answer") {
      const keys = Object.keys(reply.answers)
      if (!request.keys || keys.length !== request.keys.length || !request.keys.every(key => keys.includes(key))) {
        throw new MessageRejected("Answer every question using its exact key")
      }
    }
    request.claimed = true
    try {
      if (reply.type === "approval" && reply.allow) {
        // The fresh write is the admission fence, not an event-history lookup.
        // Failure/ambiguity propagates as unknown; it can never grant permission.
        await this.write({ type: "permission_admitted", requestId: reply.requestId, actionBinding: request.actionBinding })
      }
      if (request.signal.aborted) throw new MessageRejected("Native request was cancelled")
      request.resolve(reply)
    } catch (error) {
      request.reject(error instanceof Error ? error : new Error(String(error)))
      throw error
    }
  }
}
