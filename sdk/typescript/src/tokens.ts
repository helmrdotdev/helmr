import type {
  Duration,
  HelmrError,
  JsonValue,
  Metadata,
  WaitTimeoutError,
} from "./contract"
import { currentRuntimeOperations } from "./internal/runtime"
import { resourceID } from "./internal/id"
import {
  parsePayloadWithSchema,
  type PayloadSchemaOutput,
  type StandardSchemaV1,
} from "./schema/payload"

export interface TokenCreateRequest {
  readonly timeout?: Duration
  readonly metadata?: Metadata
  readonly tags?: readonly string[]
  readonly idempotencyKey?: string
}

export interface TokenWaitOptions {
  readonly timeout?: Duration
  readonly idleTimeout?: Duration
  readonly metadata?: Metadata
  readonly tags?: readonly string[]
}

export type TokenStatus = "pending" | "completed" | "cancelled" | "expired"

export interface Token {
  readonly id: string
  readonly status: TokenStatus
  readonly result?: JsonValue
  readonly metadata: Metadata
  readonly tags: readonly string[]
  readonly timeoutAt: string
  readonly completedAt?: string
  readonly createdAt: string
  readonly updatedAt: string
}

export interface TokenListItem {
  readonly id: string
  readonly status: TokenStatus
  readonly tags: readonly string[]
  readonly timeoutAt: string
  readonly completedAt?: string
  readonly createdAt: string
  readonly updatedAt: string
}

export interface TokenListQuery {
  readonly status?: TokenStatus
  readonly cursor?: string
  readonly limit?: number
}

export interface TokenCancelledError extends HelmrError {
  readonly code: "token_cancelled"
}

export interface TokenExpiredError extends HelmrError {
  readonly code: "token_expired"
}

export type TokenWaitError =
  | WaitTimeoutError
  | TokenCancelledError
  | TokenExpiredError

export type TokenWaitResult<T> =
  | Readonly<{ ok: true; data: T; unwrap(): T }>
  | Readonly<{ ok: false; error: TokenWaitError; unwrap(): never }>

export interface TokenWait<T> extends PromiseLike<TokenWaitResult<T>> {
  unwrap(): Promise<T>
}

export interface TokenCreateResult extends Token {
  readonly status: "pending"
  readonly callbackUrl: string
  readonly publicAccessToken: string
}

export interface TokenRef {
  readonly id: string
  wait(options?: TokenWaitOptions): TokenWait<JsonValue>
  wait<TSchema extends StandardSchemaV1<any, any>>(
    options: TokenWaitOptions & Readonly<{ schema: TSchema }>,
  ): TokenWait<PayloadSchemaOutput<TSchema>>
}

interface Tokens {
  create(request?: TokenCreateRequest): Promise<TokenCreateResult & TokenRef>
  ref(id: string): TokenRef
}

export const tokens: Tokens = Object.freeze({
  async create(request: TokenCreateRequest = {}): Promise<TokenCreateResult & TokenRef> {
    const token = await currentRuntimeOperations().tokenCreate(request)
    return Object.freeze({
      ...token,
      wait: tokenWait(token.id),
    }) as TokenCreateResult & TokenRef
  },
  ref(id: string): TokenRef {
    const normalized = resourceID(id, "Token ID")
    return Object.freeze({
      id: normalized,
      wait: tokenWait(normalized),
    })
  },
})

function tokenWait(tokenId: string): TokenRef["wait"] {
  const wait = <TSchema extends StandardSchemaV1<any, any>>(
    waitOptions: (TokenWaitOptions & Readonly<{ schema?: TSchema }>) = {},
  ): TokenWait<JsonValue | PayloadSchemaOutput<TSchema>> => {
    const pending = currentRuntimeOperations()
      .tokenWait(tokenId, waitOptions)
      .then(async (value) => {
        const parsed = waitOptions.schema === undefined
          ? value
          : await parsePayloadWithSchema(
            waitOptions.schema,
            value,
            "Token completion result",
          )
        return Object.freeze({
          ok: true as const,
          data: parsed,
          unwrap: () => parsed,
        })
      })
      .catch((error: unknown) =>
        Object.freeze({
          ok: false as const,
          error: tokenWaitError(error),
          unwrap(): never {
            throw this.error
          },
        })
      )
    return Object.freeze({
      then<TResult1 = TokenWaitResult<JsonValue | PayloadSchemaOutput<TSchema>>, TResult2 = never>(
        onfulfilled?: ((value: TokenWaitResult<JsonValue | PayloadSchemaOutput<TSchema>>) => TResult1 | PromiseLike<TResult1>) | null,
        onrejected?: ((reason: unknown) => TResult2 | PromiseLike<TResult2>) | null,
      ): PromiseLike<TResult1 | TResult2> {
        return pending.then(onfulfilled, onrejected)
      },
      async unwrap(): Promise<JsonValue | PayloadSchemaOutput<TSchema>> {
        const result = await pending
        if (!result.ok) throw result.error
        return result.data
      },
    })
  }
  return wait as TokenRef["wait"]
}

function tokenWaitError(value: unknown): TokenWaitError {
  if (
    value instanceof Error &&
    "code" in value &&
    (value.code === "wait_timeout" ||
      value.code === "token_cancelled" ||
      value.code === "token_expired")
  ) {
    return value as TokenWaitError
  }
  throw value
}
