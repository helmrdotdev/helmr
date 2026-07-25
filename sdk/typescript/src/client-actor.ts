import type {
  ActorFailure,
  ActorOperationReceipt,
  ActorOutputRecord,
  ActorStartOptions,
  ActorStatus,
  JsonValue,
  RunHandle,
} from "./contract"
import type { ActorDefinition } from "./contract"
import type { RequestOptions } from "./request"
import { validateTaskId } from "./schema/task"

export type ClientActorStartRequest = Omit<ActorStartOptions, "signal">

export interface ClientActorOutputQuery {
  readonly after?: number
  readonly limit?: number
}

export interface ClientActorInputRef {
  send(
    input: JsonValue,
    request?: Readonly<{ idempotencyKey?: string }>,
    options?: RequestOptions,
  ): Promise<{ sequence: number }>
}

export interface ClientActorOutputRef {
  list(
    query?: ClientActorOutputQuery,
    options?: RequestOptions,
  ): Promise<readonly ActorOutputRecord[]>
  read(
    query?: ClientActorOutputQuery,
    options?: RequestOptions,
  ): AsyncIterable<ActorOutputRecord>
}

export interface ClientActorRefBase {
  readonly input: ClientActorInputRef
  readonly output: ClientActorOutputRef
  status(options?: RequestOptions): Promise<ActorStatus>
  close(
    request?: Readonly<{ idempotencyKey?: string }>,
    options?: RequestOptions,
  ): Promise<ActorOperationReceipt>
}

export type ClientActorIdRef =
  & ClientActorRefBase
  & Readonly<{ id: string; key?: never }>

export type ClientActorKeyRef =
  & ClientActorRefBase
  & Readonly<{ id?: never; key: string }>

export interface ClientActorsApi {
  start<TActor extends ActorDefinition>(
    actorDeclaredId: string,
    request: ClientActorStartRequest,
    options?: RequestOptions,
  ): Promise<{ ref: ClientActorIdRef; run: RunHandle }>
  ref<TActor extends ActorDefinition>(
    actorDeclaredId: string,
    address: Readonly<{ id: string; key?: never }>,
  ): ClientActorIdRef
  ref<TActor extends ActorDefinition>(
    actorDeclaredId: string,
    address: Readonly<{ id?: never; key: string }>,
  ): ClientActorKeyRef
}

interface ActorTransport {
  request(
    method: "GET" | "POST",
    path: string,
    options?: Readonly<{ body?: unknown; signal?: AbortSignal }>,
  ): Promise<unknown>
}

export function createClientActors(
  transport: ActorTransport,
): ClientActorsApi {
  function ref<TActor extends ActorDefinition>(
    actorDeclaredId: string,
    address:
      | Readonly<{ id: string; key?: never }>
      | Readonly<{ id?: never; key: string }>,
  ): ClientActorIdRef | ClientActorKeyRef {
    validateTaskId(actorDeclaredId)
    validateActorAddress(address)
    return createClientActorRef(actorDeclaredId, address, transport)
  }
  return Object.freeze({
    async start<TActor extends ActorDefinition>(
      actorDeclaredId: string,
      request: ClientActorStartRequest,
      options: RequestOptions = {},
    ): Promise<{ ref: ClientActorIdRef; run: RunHandle }> {
      validateTaskId(actorDeclaredId)
      const response = actorObject(
        await transport.request(
          "POST",
          `/api/actors/${encodeURIComponent(actorDeclaredId)}/start`,
          {
            body: actorStartBody(request),
            ...(options.signal === undefined ? {} : { signal: options.signal }),
          },
        ),
        "Actor start response",
      )
      const actorID = actorPublicID(response["actor_id"])
      const runID = runPublicID(response["run_id"])
      return Object.freeze({
        ref: createClientActorRef(
          actorDeclaredId,
          { id: actorID },
          transport,
        ) as ClientActorIdRef,
        run: Object.freeze({ id: runID }),
      })
    },
    ref,
  }) as ClientActorsApi
}

function createClientActorRef(
  actorDeclaredId: string,
  address:
    | Readonly<{ id: string; key?: never }>
    | Readonly<{ id?: never; key: string }>,
  transport: ActorTransport,
): ClientActorIdRef | ClientActorKeyRef {
  const addressBody = () =>
    "id" in address && address.id !== undefined
      ? { actor_id: address.id }
      : { actor_key: address.key }
  const addressQuery = () => new URLSearchParams(addressBody()).toString()
  const readPage = async (
    query: ClientActorOutputQuery,
    options: RequestOptions,
  ): Promise<{
    records: readonly ActorOutputRecord[]
    nextAfter: number
    hasMore: boolean
  }> => {
    const values = new URLSearchParams(addressBody())
    if (query.after !== undefined) values.set("after", String(query.after))
    if (query.limit !== undefined) values.set("limit", String(query.limit))
    const response = actorObject(
      await transport.request(
        "GET",
        `/api/actors/${encodeURIComponent(actorDeclaredId)}/output?${values}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ),
      "Actor output response",
    )
    if (!Array.isArray(response["records"])) {
      throw new Error("Actor output response.records must be an array")
    }
    return {
      records: Object.freeze(response["records"].map(parseActorOutputRecord)),
      nextAfter: safeSequence(response["next_after"], "Actor output next_after"),
      hasMore: requiredBoolean(response, "has_more", "Actor output response"),
    }
  }
  const output: ClientActorOutputRef = Object.freeze({
    async list(
      query: ClientActorOutputQuery = {},
      options: RequestOptions = {},
    ): Promise<readonly ActorOutputRecord[]> {
      return (await readPage(query, options)).records
    },
    read(
      query: ClientActorOutputQuery = {},
      options: RequestOptions = {},
    ): AsyncIterable<ActorOutputRecord> {
      return {
        async *[Symbol.asyncIterator]() {
          let after = query.after
          while (true) {
            options.signal?.throwIfAborted()
            const page = await readPage(
              {
                ...(after === undefined ? {} : { after }),
                ...(query.limit === undefined ? {} : { limit: query.limit }),
              },
              options,
            )
            for (const record of page.records) {
              options.signal?.throwIfAborted()
              yield record
            }
            if (!page.hasMore) return
            after = page.nextAfter
          }
        },
      }
    },
  })
  const value: ClientActorRefBase = {
    input: Object.freeze({
      async send(
        input: JsonValue,
        request: Readonly<{ idempotencyKey?: string }> = {},
        options: RequestOptions = {},
      ): Promise<{ sequence: number }> {
        const response = actorObject(
          await transport.request(
            "POST",
            `/api/actors/${encodeURIComponent(actorDeclaredId)}/input`,
            {
              body: {
                ...addressBody(),
                input,
                ...(request.idempotencyKey === undefined
                  ? {}
                  : { idempotency_key: request.idempotencyKey }),
              },
              ...(options.signal === undefined ? {} : { signal: options.signal }),
            },
          ),
          "Actor input response",
        )
        return Object.freeze({
          sequence: safeSequence(response["sequence"], "Actor input sequence"),
        })
      },
    }),
    output,
    async status(options: RequestOptions = {}): Promise<ActorStatus> {
      return parseActorStatus(
        await transport.request(
          "GET",
          `/api/actors/${encodeURIComponent(actorDeclaredId)}/status?${addressQuery()}`,
          options.signal === undefined ? {} : { signal: options.signal },
        ),
      )
    },
    async close(
      request: Readonly<{ idempotencyKey?: string }> = {},
      options: RequestOptions = {},
    ): Promise<ActorOperationReceipt> {
      const response = actorObject(
        await transport.request(
          "POST",
          `/api/actors/${encodeURIComponent(actorDeclaredId)}/close`,
          {
            body: {
              ...addressBody(),
              ...(request.idempotencyKey === undefined
                ? {}
                : { idempotency_key: request.idempotencyKey }),
            },
            ...(options.signal === undefined ? {} : { signal: options.signal }),
          },
        ),
        "Actor close response",
      )
      return Object.freeze({
        actorId: actorPublicID(response["actor_id"]),
        acceptedAt: timestamp(response["accepted_at"], "Actor close accepted_at"),
      })
    },
  }
  return Object.freeze({ ...address, ...value }) as
    | ClientActorIdRef
    | ClientActorKeyRef
}

function actorStartBody(request: ClientActorStartRequest): Record<string, unknown> {
  return {
    ...(request.key === undefined ? {} : { key: request.key }),
    ...(request.input === undefined ? {} : { input: request.input }),
    ...(request.idempotencyKey === undefined
      ? {}
      : { idempotency_key: request.idempotencyKey }),
    workspace: "id" in request.workspace
      ? { id: request.workspace.id }
      : { key: request.workspace.key },
    ...(request.run === undefined
      ? {}
      : {
          run: {
            ...(request.run.queue === undefined ? {} : { queue: request.run.queue }),
            ...(request.run.concurrencyKey === undefined
              ? {}
              : { concurrency_key: request.run.concurrencyKey }),
            ...(request.run.priority === undefined
              ? {}
              : { priority: request.run.priority }),
            ...(request.run.ttl === undefined ? {} : { ttl: request.run.ttl }),
            ...(request.run.retry === undefined
              ? {}
              : {
                  retry: request.run.retry.enabled === false
                    ? { enabled: false }
                    : {
                        ...(request.run.retry.enabled === undefined
                          ? {}
                          : { enabled: request.run.retry.enabled }),
                        max_attempts: request.run.retry.maxAttempts,
                        ...(request.run.retry.backoff === undefined
                          ? {}
                          : {
                              backoff: {
                                ...(request.run.retry.backoff.minDelay === undefined
                                  ? {}
                                  : { min_delay: request.run.retry.backoff.minDelay }),
                                ...(request.run.retry.backoff.maxDelay === undefined
                                  ? {}
                                  : { max_delay: request.run.retry.backoff.maxDelay }),
                                ...(request.run.retry.backoff.factor === undefined
                                  ? {}
                                  : { factor: request.run.retry.backoff.factor }),
                                ...(request.run.retry.backoff.jitter === undefined
                                  ? {}
                                  : { jitter: request.run.retry.backoff.jitter }),
                              },
                            }),
                      },
                }),
            ...(request.run.metadata === undefined
              ? {}
              : { metadata: request.run.metadata }),
            ...(request.run.tags === undefined
              ? {}
              : { tags: [...request.run.tags] }),
          },
        }),
  }
}

function parseActorStatus(value: unknown): ActorStatus {
  const input = actorObject(value, "Actor status response")
  const status = input["status"]
  if (
    status !== "open" &&
    status !== "closed" &&
    status !== "cancelled" &&
    status !== "failed"
  ) {
    throw new Error("Actor status response.status is invalid")
  }
  const failure = input["failure"] === undefined
    ? undefined
    : parseActorFailure(input["failure"])
  return Object.freeze({
    id: actorPublicID(input["id"]),
    ...(input["key"] === undefined
      ? {}
      : { key: requiredString(input, "key", "Actor status response") }),
    status,
    createdAt: timestamp(input["created_at"], "Actor status created_at"),
    updatedAt: timestamp(input["updated_at"], "Actor status updated_at"),
    ...(input["current_run_id"] === undefined
      ? {}
      : {
          currentRunId: runPublicID(
            requiredString(input, "current_run_id", "Actor status response"),
          ),
        }),
    ...(failure === undefined ? {} : { failure }),
  })
}

function parseActorFailure(value: unknown): ActorFailure {
  const input = actorObject(value, "Actor failure")
  const code = requiredString(input, "code", "Actor failure")
  if (
    code !== "no-progress" &&
    code !== "run-failed" &&
    code !== "run-expired" &&
    code !== "platform-failure"
  ) {
    throw new Error("Actor failure.code is invalid")
  }
  return Object.freeze({
    code,
    runId: runPublicID(requiredString(input, "run_id", "Actor failure")),
  })
}

function parseActorOutputRecord(value: unknown): ActorOutputRecord {
  const input = actorObject(value, "Actor output record")
  const provenance = actorObject(input["provenance"], "Actor output provenance")
  return Object.freeze({
    id: recordPublicID(input["id"]),
    sequence: safeSequence(input["sequence"], "Actor output sequence"),
    data: input["data"],
    contentType: requiredString(input, "content_type", "Actor output record"),
    createdAt: timestamp(input["created_at"], "Actor output created_at").toISOString(),
    provenance: Object.freeze({
      runId: runPublicID(requiredString(provenance, "run_id", "Actor output provenance")),
      attemptNumber: safeSequence(
        provenance["attempt_number"],
        "Actor output attempt_number",
      ),
      deploymentId: requiredString(
        provenance,
        "deployment_id",
        "Actor output provenance",
      ),
    }),
  })
}

function validateActorAddress(
  address:
    | Readonly<{ id: string; key?: never }>
    | Readonly<{ id?: never; key: string }>,
): void {
  if (
    ("id" in address && typeof address.id === "string") ===
    ("key" in address && typeof address.key === "string")
  ) {
    throw new Error("Actor ref requires exactly one of id or key")
  }
  if ("id" in address && address.id !== undefined) {
    actorPublicID(address.id)
  } else if (address.key.length === 0) {
    throw new Error("Actor key is required")
  }
}

function actorObject(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}

function requiredString(
  value: Record<string, unknown>,
  field: string,
  label: string,
): string {
  const result = value[field]
  if (typeof result !== "string" || result === "") {
    throw new Error(`${label}.${field} must be a non-empty string`)
  }
  return result
}

function requiredBoolean(
  value: Record<string, unknown>,
  field: string,
  label: string,
): boolean {
  const result = value[field]
  if (typeof result !== "boolean") {
    throw new Error(`${label}.${field} must be a boolean`)
  }
  return result
}

function safeSequence(value: unknown, label: string): number {
  if (!Number.isSafeInteger(value) || (value as number) < 0) {
    throw new Error(`${label} must be a non-negative safe integer`)
  }
  return value as number
}

function timestamp(value: unknown, label: string): Date {
  if (typeof value !== "string") throw new Error(`${label} must be a timestamp`)
  const result = new Date(value)
  if (Number.isNaN(result.valueOf())) throw new Error(`${label} must be a timestamp`)
  return result
}

function actorPublicID(value: unknown): string {
  if (typeof value !== "string" || !/^act_[a-z2-7]{26}$/.test(value)) {
    throw new Error("Actor ID must be a canonical act_ public ID")
  }
  return value
}

function runPublicID(value: unknown): string {
  if (typeof value !== "string" || !/^run_[a-z2-7]{26}$/.test(value)) {
    throw new Error("Run ID must be a canonical run_ public ID")
  }
  return value
}

function recordPublicID(value: unknown): string {
  if (typeof value !== "string" || !/^arec_[a-z2-7]{26}$/.test(value)) {
    throw new Error("Actor record ID must be a canonical arec_ public ID")
  }
  return value
}
