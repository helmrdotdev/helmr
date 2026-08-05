import type { CursorPage } from "./contract"
import { resourceID } from "./internal/id"
import type { RequestOptions } from "./request"
import { validateSecretName } from "./secret"

export type SecretValue = string
export type SecretStatus = "active" | "revoked"

export interface SecretSnapshot {
  readonly id: string
  readonly name: string
  readonly status: SecretStatus
  readonly createdAt: Date
  readonly rotatedAt?: Date
  readonly revokedAt?: Date
}

export interface SecretRef {
  readonly id: string
  retrieve(options?: RequestOptions): Promise<SecretSnapshot>
  rotate(
    request: Readonly<{ value: SecretValue; idempotencyKey: string }>,
    options?: RequestOptions,
  ): Promise<SecretSnapshot>
  revoke(
    request: Readonly<{ idempotencyKey: string }>,
    options?: RequestOptions,
  ): Promise<SecretSnapshot>
}

export interface ClientSecretsApi {
  create(
    request: Readonly<{
      name: string
      value: SecretValue
      idempotencyKey?: string
    }>,
    options?: RequestOptions,
  ): Promise<SecretSnapshot>
  ref(id: string): SecretRef
  list(
    query?: Readonly<{ cursor?: string; limit?: number; name?: string }>,
    options?: RequestOptions,
  ): Promise<CursorPage<SecretSnapshot>>
}

interface SecretTransport {
  request(
    method: "GET" | "POST",
    path: string,
    options?: Readonly<{ body?: unknown; signal?: AbortSignal }>,
  ): Promise<unknown>
}

export function createClientSecrets(
  transport: SecretTransport,
): ClientSecretsApi {
  return Object.freeze({
    async create(
      request: Readonly<{
        name: string
        value: SecretValue
        idempotencyKey?: string
      }>,
      options: RequestOptions = {},
    ): Promise<SecretSnapshot> {
      validateSecretName(request.name)
      if (typeof request.value !== "string") {
        throw new Error("Secret create request.value must be a string")
      }
      validateOptionalIdempotencyKey(request.idempotencyKey)
      return parseSecretSnapshot(
        await transport.request("POST", "/v1/secrets", {
          body: {
            name: request.name,
            value: request.value,
            ...(request.idempotencyKey === undefined
              ? {}
              : { idempotency_key: request.idempotencyKey }),
          },
          ...(options.signal === undefined ? {} : { signal: options.signal }),
        }),
      )
    },
    ref(id: string): SecretRef {
      return createSecretRef(id, transport)
    },
    async list(
      queryInput: Readonly<{ cursor?: string; limit?: number; name?: string }> = {},
      options: RequestOptions = {},
    ): Promise<CursorPage<SecretSnapshot>> {
      const query = new URLSearchParams()
      if (queryInput.name !== undefined) {
        validateSecretName(queryInput.name)
        if (queryInput.cursor !== undefined || queryInput.limit !== undefined) {
          throw new Error("Secret exact name lookup does not accept cursor or limit")
        }
        query.set("name", queryInput.name)
      }
      if (queryInput.cursor !== undefined) {
        if (queryInput.cursor.trim() === "") {
          throw new Error("Secret list query.cursor must not be empty")
        }
        query.set("cursor", queryInput.cursor)
      }
      if (queryInput.limit !== undefined) {
        if (
          !Number.isInteger(queryInput.limit) ||
          queryInput.limit < 1 ||
          queryInput.limit > 100
        ) {
          throw new Error("Secret list query.limit must be an integer in [1,100]")
        }
        query.set("limit", String(queryInput.limit))
      }
      const suffix = query.size === 0 ? "" : `?${query.toString()}`
      const response = objectValue(
        await transport.request("GET", `/v1/secrets${suffix}`, {
          ...(options.signal === undefined ? {} : { signal: options.signal }),
        }),
        "Secret list response",
      )
      if (!Array.isArray(response["secrets"])) {
        throw new Error("Secret list response.secrets must be an array")
      }
      const nextCursor = response["next_cursor"]
      if (nextCursor !== undefined && typeof nextCursor !== "string") {
        throw new Error("Secret list response.next_cursor must be a string")
      }
      return Object.freeze({
        items: Object.freeze(
          response["secrets"].map((value) => parseSecretSnapshot(value)),
        ),
        ...(nextCursor === undefined ? {} : { nextCursor }),
      })
    },
  })
}

function createSecretRef(
  id: string,
  transport: SecretTransport,
): SecretRef {
  const secretID = resourceID(id, "Secret ID")
  const path = `/v1/secrets/${encodeURIComponent(secretID)}`
  return Object.freeze({
    id: secretID,
    async retrieve(options: RequestOptions = {}): Promise<SecretSnapshot> {
      return parseSecretSnapshot(
        await transport.request(
          "GET",
          path,
          options.signal === undefined ? {} : { signal: options.signal },
        ),
      )
    },
    async rotate(
      request: Readonly<{ value: SecretValue; idempotencyKey: string }>,
      options: RequestOptions = {},
    ): Promise<SecretSnapshot> {
      if (typeof request.value !== "string") {
        throw new Error("Secret rotate request.value must be a string")
      }
      validateIdempotencyKey(request.idempotencyKey)
      return parseSecretSnapshot(
        await transport.request("POST", `${path}/rotate`, {
          body: {
            value: request.value,
            idempotency_key: request.idempotencyKey,
          },
          ...(options.signal === undefined ? {} : { signal: options.signal }),
        }),
      )
    },
    async revoke(
      request: Readonly<{ idempotencyKey: string }>,
      options: RequestOptions = {},
    ): Promise<SecretSnapshot> {
      validateIdempotencyKey(request.idempotencyKey)
      return parseSecretSnapshot(
        await transport.request("POST", `${path}/revoke`, {
          body: { idempotency_key: request.idempotencyKey },
          ...(options.signal === undefined ? {} : { signal: options.signal }),
        }),
      )
    },
  })
}

function parseSecretSnapshot(value: unknown): SecretSnapshot {
  const input = objectValue(value, "Secret response")
  const id = input["id"]
  const name = input["name"]
  const status = input["status"]
  if (typeof id !== "string") throw new Error("Secret response.id must be a string")
  if (typeof name !== "string") {
    throw new Error("Secret response.name must be a string")
  }
  if (status !== "active" && status !== "revoked") {
    throw new Error("Secret response.status must be active or revoked")
  }
  const canonicalID = resourceID(id, "Secret response.id")
  validateSecretName(name)
  return Object.freeze({
    id: canonicalID,
    name,
    status,
    createdAt: dateValue(input["created_at"], "Secret response.created_at"),
    ...optionalDate(input, "rotated_at", "rotatedAt"),
    ...optionalDate(input, "revoked_at", "revokedAt"),
  })
}

function optionalDate(
  input: Record<string, unknown>,
  wireName: string,
  fieldName: "rotatedAt" | "revokedAt",
): Partial<Pick<SecretSnapshot, "rotatedAt" | "revokedAt">> {
  const value = input[wireName]
  return value === undefined || value === null
    ? {}
    : { [fieldName]: dateValue(value, `Secret response.${wireName}`) }
}

function dateValue(value: unknown, label: string): Date {
  if (typeof value !== "string") throw new Error(`${label} must be a string`)
  const parsed = new Date(value)
  if (Number.isNaN(parsed.getTime())) {
    throw new Error(`${label} must be an RFC 3339 timestamp`)
  }
  return parsed
}

function validateOptionalIdempotencyKey(value: string | undefined): void {
  if (value !== undefined) validateIdempotencyKey(value)
}

function validateIdempotencyKey(value: string): void {
  if (typeof value !== "string" || value.trim() === "") {
    throw new Error("Secret idempotencyKey is required")
  }
}

function objectValue(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}
