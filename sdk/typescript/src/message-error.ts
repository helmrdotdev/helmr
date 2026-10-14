const rejectedBrand = Symbol.for("helmr.sdk.MessageRejected")

/** A known rejection before uncertain application effects. */
export class MessageRejected extends Error {
  constructor(message: string) {
    super(message)
    this.name = "MessageRejected"
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
