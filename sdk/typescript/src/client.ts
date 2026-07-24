import type { CursorPage, JsonValue, Metadata } from "./contract"
import type {
  TokenCreateOptions,
  TokenSnapshot,
} from "./tokens"
import {
  HELMR_API_VERSION,
  HELMR_API_VERSION_HEADER,
  HELMR_SDK_VERSION,
  HELMR_SDK_VERSION_HEADER,
} from "./version"

export interface HelmrClientOptions {
  readonly url: string
  readonly apiKey: string
  readonly fetch?: typeof fetch
}

export interface ClientTokenCreateOptions extends TokenCreateOptions {
  readonly signal?: AbortSignal
}

export interface ClientTokenCreateResult extends TokenSnapshot {
  readonly status: "pending"
  readonly callbackUrl: string
  readonly publicAccessToken: string
}

export interface ClientTokensApi {
  create(options?: ClientTokenCreateOptions): Promise<ClientTokenCreateResult>
  retrieve(id: string, options?: Readonly<{ signal?: AbortSignal }>): Promise<TokenSnapshot>
  list(options?: Readonly<{
    cursor?: string
    limit?: number
    signal?: AbortSignal
  }>): Promise<CursorPage<TokenSnapshot>>
  complete(id: string, result: JsonValue, options?: Readonly<{
    idempotencyKey?: string
    signal?: AbortSignal
  }>): Promise<TokenSnapshot>
  cancel(id: string, options?: Readonly<{
    idempotencyKey?: string
    signal?: AbortSignal
  }>): Promise<TokenSnapshot>
}

export class HelmrClient {
  readonly tokens: ClientTokensApi

  constructor(options: HelmrClientOptions) {
    const transport = new ClientTransport(options)
    this.tokens = Object.freeze(new ClientTokens(transport))
  }
}

class ClientTokens implements ClientTokensApi {
  readonly #transport: ClientTransport

  constructor(transport: ClientTransport) {
    this.#transport = transport
  }

  async create(options: ClientTokenCreateOptions = {}): Promise<ClientTokenCreateResult> {
    const response = await this.#transport.request("POST", "/api/tokens", {
      body: {
        ...(options.timeout === undefined ? {} : { timeout: options.timeout }),
        ...(options.metadata === undefined ? {} : { metadata: options.metadata }),
        ...(options.tags === undefined ? {} : { tags: [...options.tags] }),
        ...(options.idempotencyKey === undefined
          ? {}
          : { idempotency_key: options.idempotencyKey }),
      },
      ...(options.signal === undefined ? {} : { signal: options.signal }),
    })
    const token = parseToken(response, true)
    if (token.status !== "pending") {
      throw new Error("Token create response must be pending")
    }
    return token as ClientTokenCreateResult
  }

  async retrieve(
    id: string,
    options: Readonly<{ signal?: AbortSignal }> = {},
  ): Promise<TokenSnapshot> {
    return parseToken(
      await this.#transport.request(
        "GET",
        `/api/tokens/${encodeURIComponent(tokenID(id))}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ),
      false,
    )
  }

  async list(options: Readonly<{
    cursor?: string
    limit?: number
    signal?: AbortSignal
  }> = {}): Promise<CursorPage<TokenSnapshot>> {
    const query = new URLSearchParams()
    if (options.cursor !== undefined) query.set("cursor", options.cursor)
    if (options.limit !== undefined) query.set("limit", String(options.limit))
    const suffix = query.size === 0 ? "" : `?${query.toString()}`
    const response = objectValue(
      await this.#transport.request("GET", `/api/tokens${suffix}`, {
        ...(options.signal === undefined ? {} : { signal: options.signal }),
      }),
      "Token list response",
    )
    if (!Array.isArray(response["tokens"])) {
      throw new Error("Token list response.tokens must be an array")
    }
    const nextCursor = response["next_cursor"]
    if (nextCursor !== undefined && typeof nextCursor !== "string") {
      throw new Error("Token list response.next_cursor must be a string")
    }
    return Object.freeze({
      items: Object.freeze(response["tokens"].map((token) => parseToken(token, false))),
      ...(nextCursor === undefined ? {} : { nextCursor }),
    })
  }

  async complete(
    id: string,
    result: JsonValue,
    options: Readonly<{ idempotencyKey?: string; signal?: AbortSignal }> = {},
  ): Promise<TokenSnapshot> {
    const response = objectValue(
      await this.#transport.request(
        "POST",
        `/api/tokens/${encodeURIComponent(tokenID(id))}/complete`,
        {
          body: {
            result,
            ...(options.idempotencyKey === undefined
              ? {}
              : { idempotency_key: options.idempotencyKey }),
          },
          ...(options.signal === undefined ? {} : { signal: options.signal }),
        },
      ),
      "Token completion response",
    )
    return parseToken(response["token"], false)
  }

  async cancel(
    id: string,
    options: Readonly<{ idempotencyKey?: string; signal?: AbortSignal }> = {},
  ): Promise<TokenSnapshot> {
    return parseToken(
      await this.#transport.request(
        "POST",
        `/api/tokens/${encodeURIComponent(tokenID(id))}/cancel`,
        {
          body: options.idempotencyKey === undefined
            ? {}
            : { idempotency_key: options.idempotencyKey },
          ...(options.signal === undefined ? {} : { signal: options.signal }),
        },
      ),
      false,
    )
  }
}

class ClientTransport {
  readonly #baseURL: URL
  readonly #apiKey: string
  readonly #fetch: typeof fetch

  constructor(options: HelmrClientOptions) {
    this.#baseURL = clientBaseURL(options.url)
    this.#apiKey = options.apiKey.trim()
    if (this.#apiKey === "") throw new Error("Helmr API key is required")
    this.#fetch = options.fetch ?? globalThis.fetch
    if (typeof this.#fetch !== "function") {
      throw new Error("fetch is unavailable")
    }
  }

  async request(
    method: "GET" | "POST",
    path: string,
    options: Readonly<{ body?: unknown; signal?: AbortSignal }> = {},
  ): Promise<unknown> {
    const response = await this.#fetch(new URL(path, this.#baseURL), {
      method,
      headers: {
        Authorization: `Bearer ${this.#apiKey}`,
        [HELMR_API_VERSION_HEADER]: HELMR_API_VERSION,
        [HELMR_SDK_VERSION_HEADER]: HELMR_SDK_VERSION,
        ...(options.body === undefined ? {} : { "Content-Type": "application/json" }),
      },
      ...(options.body === undefined ? {} : { body: JSON.stringify(options.body) }),
      ...(options.signal === undefined ? {} : { signal: options.signal }),
    })
    const value: unknown = await response.json().catch(() => ({}))
    if (!response.ok) {
      throw clientRequestError(response, value)
    }
    return value
  }
}

function clientRequestError(response: Response, value: unknown): Error {
  const body = value !== null && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown>
    : {}
  const message = typeof body["error"] === "string" && body["error"] !== ""
    ? body["error"]
    : typeof body["message"] === "string" && body["message"] !== ""
    ? body["message"]
    : `Helmr request failed with status ${response.status}`
  const code = typeof body["code"] === "string" && body["code"] !== ""
    ? body["code"]
    : responseStatusCode(response.status)
  const retryable = typeof body["retryable"] === "boolean"
    ? body["retryable"]
    : response.status === 429 || response.status >= 500
  const requestId = typeof body["request_id"] === "string" && body["request_id"] !== ""
    ? body["request_id"]
    : typeof body["requestId"] === "string" && body["requestId"] !== ""
    ? body["requestId"]
    : undefined
  const error = new Error(message) as Error & {
    code: string
    retryable: boolean
    requestId?: string
  }
  error.name = "HelmrError"
  error.code = code
  error.retryable = retryable
  if (requestId !== undefined) error.requestId = requestId
  return error
}

function responseStatusCode(status: number): string {
  switch (status) {
    case 400:
      return "bad_request"
    case 401:
      return "unauthorized"
    case 403:
      return "forbidden"
    case 404:
      return "not_found"
    case 409:
      return "conflict"
    case 410:
      return "gone"
    case 422:
      return "unprocessable_entity"
    case 429:
      return "rate_limited"
    default:
      return status >= 500 ? "internal_error" : "request_failed"
  }
}

function clientBaseURL(raw: string): URL {
  const url = new URL(raw)
  if (url.search !== "" || url.hash !== "") {
    throw new Error("Helmr API URL must not include query or fragment")
  }
  if (url.protocol !== "https:" && !(
    url.protocol === "http:" &&
    (url.hostname === "localhost" || url.hostname === "127.0.0.1" || url.hostname === "[::1]")
  )) {
    throw new Error("Helmr API URL must use HTTPS except on loopback")
  }
  if (!url.pathname.endsWith("/")) url.pathname += "/"
  return url
}

function tokenID(value: string): string {
  const normalized = value.trim()
  if (!/^tok_[a-z2-7]{26}$/.test(normalized)) {
    throw new Error("Token ID must be a canonical tok_ public ID")
  }
  return normalized
}

function parseToken(value: unknown, credentials: boolean): TokenSnapshot | ClientTokenCreateResult {
  const token = objectValue(value, "Token response")
  const status = token["status"]
  if (
    status !== "pending" &&
    status !== "completed" &&
    status !== "cancelled" &&
    status !== "expired"
  ) {
    throw new Error("Token response.status is invalid")
  }
  const metadata = objectValue(token["metadata"], "Token response.metadata") as Metadata
  const tags = token["tags"]
  if (!Array.isArray(tags) || tags.some((tag) => typeof tag !== "string")) {
    throw new Error("Token response.tags must be an array of strings")
  }
  const snapshot: TokenSnapshot = {
    id: requiredString(token, "id"),
    status,
    ...(token["result"] === undefined ? {} : { result: token["result"] as JsonValue }),
    metadata,
    tags: Object.freeze([...tags]) as readonly string[],
    timeoutAt: requiredString(token, "timeout_at"),
    ...(token["completed_at"] === undefined
      ? {}
      : { completedAt: requiredString(token, "completed_at") }),
    createdAt: requiredString(token, "created_at"),
    updatedAt: requiredString(token, "updated_at"),
  }
  if (!credentials) return Object.freeze(snapshot)
  return Object.freeze({
    ...snapshot,
    callbackUrl: requiredString(token, "callback_url"),
    publicAccessToken: requiredString(token, "public_access_token"),
  }) as ClientTokenCreateResult
}

function objectValue(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}

function requiredString(value: Record<string, unknown>, field: string): string {
  const result = value[field]
  if (typeof result !== "string" || result === "") {
    throw new Error(`Token response.${field} must be a non-empty string`)
  }
  return result
}
