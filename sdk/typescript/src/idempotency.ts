import { createHash } from "node:crypto"

export type IdempotencyKeyScope = "global"

export type IdempotencyKeyMaterial = string | readonly string[]

export type IdempotencyKeyCreateOptions = { readonly scope?: "global" }

export interface IdempotencyKey {
  readonly value: string
  readonly key: IdempotencyKeyMaterial
  readonly scope: IdempotencyKeyScope
}

export const idempotencyKeys = {
  create(key: IdempotencyKeyMaterial, options: IdempotencyKeyCreateOptions = {}): IdempotencyKey {
    const scope = options.scope ?? "global"
    return {
      value: createIdempotencyKey(key, scope),
      key,
      scope,
    }
  },
}

function createIdempotencyKey(
  key: IdempotencyKeyMaterial,
  scope: IdempotencyKeyScope,
): string {
  const material: Record<string, unknown> = {
    scope,
    key: Array.isArray(key) ? [...key] : [key],
  }
  return createHash("sha256").update(JSON.stringify(material)).digest("hex")
}
