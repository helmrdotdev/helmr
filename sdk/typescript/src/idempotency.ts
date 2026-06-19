import { createHash } from "node:crypto"

export type IdempotencyKeyScope = "global"

export type IdempotencyKeyMaterial = string | readonly string[]

export type IdempotencyKeyCreateOptions = { readonly scope?: "global" }

export interface IdempotencyKey {
  readonly value: string
  readonly key: IdempotencyKeyMaterial
  readonly scope: IdempotencyKeyScope
}

export type IdempotencyKeyInput = IdempotencyKeyMaterial | IdempotencyKey

export interface TaskStartIdempotencyRequestFields {
  readonly idempotency_key?: string
  readonly idempotency_key_ttl?: string
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

export function taskStartIdempotencyRequestFields(
  input: IdempotencyKeyInput | undefined,
  ttl: string | undefined,
): TaskStartIdempotencyRequestFields {
  if (input === undefined) {
    return {}
  }
  const key = isIdempotencyKey(input) ? input : idempotencyKeys.create(input)
  return {
    idempotency_key: key.value,
    ...(ttl === undefined ? {} : { idempotency_key_ttl: ttl }),
  }
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

function isIdempotencyKey(value: IdempotencyKeyInput): value is IdempotencyKey {
  return typeof value === "object" && value !== null && "value" in value && "key" in value && "scope" in value
}
