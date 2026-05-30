import { createHash } from "node:crypto"

export type IdempotencyKeyScope = "global" | "run" | "attempt"

export type IdempotencyKeyMaterial = string | readonly string[]

export interface IdempotencyKeyCreateOptions {
  readonly scope?: IdempotencyKeyScope
  readonly runId?: string
  readonly attemptNumber?: number
}

export interface IdempotencyKey {
  readonly value: string
  readonly key: IdempotencyKeyMaterial
  readonly scope: IdempotencyKeyScope
}

export type IdempotencyKeyInput = IdempotencyKeyMaterial | IdempotencyKey

export interface RunIdempotencyRequestFields {
  readonly idempotency_key?: string
  readonly idempotency_key_ttl?: string
  readonly idempotency_key_options?: {
    readonly key: IdempotencyKeyMaterial
    readonly scope: IdempotencyKeyScope
  }
}

export const idempotencyKeys = {
  create(key: IdempotencyKeyMaterial, options: IdempotencyKeyCreateOptions = {}): IdempotencyKey {
    const scope = options.scope ?? "run"
    return {
      value: createIdempotencyKey(key, scope, options),
      key,
      scope,
    }
  },
}

export function runIdempotencyRequestFields(
  input: IdempotencyKeyInput | undefined,
  ttl: string | undefined,
): RunIdempotencyRequestFields {
  if (input === undefined) {
    return {}
  }
  const key = isIdempotencyKey(input) ? input : idempotencyKeys.create(input)
  return {
    idempotency_key: key.value,
    ...(ttl === undefined ? {} : { idempotency_key_ttl: ttl }),
    idempotency_key_options: {
      key: key.key,
      scope: key.scope,
    },
  }
}

function createIdempotencyKey(
  key: IdempotencyKeyMaterial,
  scope: IdempotencyKeyScope,
  options: IdempotencyKeyCreateOptions,
): string {
  const material: Record<string, unknown> = {
    scope,
    key: Array.isArray(key) ? [...key] : [key],
  }
  if (scope === "run" && options.runId !== undefined) {
    material["runId"] = options.runId
  }
  if (scope === "attempt" && options.runId !== undefined && options.attemptNumber !== undefined) {
    material["runId"] = options.runId
    material["attemptNumber"] = options.attemptNumber
  }
  return createHash("sha256").update(JSON.stringify(material)).digest("hex")
}

function isIdempotencyKey(value: IdempotencyKeyInput): value is IdempotencyKey {
  return typeof value === "object" && value !== null && "value" in value && "key" in value && "scope" in value
}
