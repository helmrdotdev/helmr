import type { Json } from "./agent"

const rejectedBrand = Symbol.for("helmr.sdk.MessageRejected")

/** A known rejection before uncertain application effects. */
export class MessageRejected extends Error {
  readonly details?: Json
  constructor(message: string, details?: Json) {
    super(message)
    this.name = "MessageRejected"
    if (details !== undefined) this.details = details
    Object.defineProperty(this, rejectedBrand, { value: true })
  }
  static override [Symbol.hasInstance](value: unknown): boolean {
    return (
      this === MessageRejected &&
      typeof value === "object" &&
      value !== null &&
      rejectedBrand in value
    )
  }
}
