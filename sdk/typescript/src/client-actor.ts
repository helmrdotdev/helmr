import type {
  ActorDefinition,
  ActorStartOptions,
  CursorPage,
  RunHandle,
  WorkspaceIdAddress,
} from "./contract"
import { resourceID } from "./internal/id"
import type { RequestOptions } from "./request"
import { validateTaskId } from "./schema/task"
import { createSessionRef, type SessionRef } from "./client-session"
import { requireWorkspaceIDAddress } from "./workspace"

export type ClientActorStartRequest = Omit<ActorStartOptions, "signal" | "workspace"> & Readonly<{
  workspace: WorkspaceIdAddress
}>

export interface ActorSnapshot {
  readonly id: string
  readonly deploymentId: string
}

export interface ActorReadQuery {
  readonly deploymentId?: string
}

export interface ActorListQuery extends ActorReadQuery {
  readonly cursor?: string
  readonly limit?: number
}

export interface ActorPage extends CursorPage<ActorSnapshot> {
  readonly deploymentId: string
}

export interface ClientActorsApi {
  retrieve(
    id: string,
    query?: ActorReadQuery,
    options?: RequestOptions,
  ): Promise<ActorSnapshot>
  list(
    query?: ActorListQuery,
    options?: RequestOptions,
  ): Promise<ActorPage>
  start<TActor extends ActorDefinition>(
    id: string,
    request: ClientActorStartRequest,
    options?: RequestOptions,
  ): Promise<{ session: SessionRef; run: RunHandle }>
}

interface ActorTransport {
  request(
    method: "GET" | "POST",
    path: string,
    options?: Readonly<{ body?: unknown; signal?: AbortSignal }>,
  ): Promise<unknown>
}

export function createClientActors(transport: ActorTransport): ClientActorsApi {
  return Object.freeze({
    async retrieve(
      id: string,
      query: ActorReadQuery = {},
      options: RequestOptions = {},
    ): Promise<ActorSnapshot> {
      validateTaskId(id)
      return parseActorSnapshot(await transport.request(
        "GET",
        `/v1/actors/${encodeURIComponent(id)}${definitionQuery(query)}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ))
    },
    async list(
      queryInput: ActorListQuery = {},
      options: RequestOptions = {},
    ): Promise<ActorPage> {
      const response = objectValue(await transport.request(
        "GET",
        `/v1/actors${definitionQuery(queryInput)}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ), "Actor list response")
      if (!Array.isArray(response["actors"])) {
        throw new Error("Actor list response.actors must be an array")
      }
      const nextCursor = response["next_cursor"]
      if (nextCursor !== undefined && typeof nextCursor !== "string") {
        throw new Error("Actor list response.next_cursor must be a string")
      }
      return Object.freeze({
        deploymentId: resourceID(response["deployment_id"], "Actor list response.deployment_id"),
        items: Object.freeze(response["actors"].map(parseActorSnapshot)),
        ...(nextCursor === undefined ? {} : { nextCursor }),
      })
    },
    async start<TActor extends ActorDefinition>(
      id: string,
      request: ClientActorStartRequest,
      options: RequestOptions = {},
    ): Promise<{ session: SessionRef; run: RunHandle }> {
      validateTaskId(id)
      const response = objectValue(await transport.request(
        "POST",
        `/v1/actors/${encodeURIComponent(id)}/start`,
        {
          body: actorStartBody(request),
          ...(options.signal === undefined ? {} : { signal: options.signal }),
        },
      ), "Actor start response")
      const sessionID = resourceID(response["session_id"], "Actor start response.session_id")
      const runID = resourceID(response["run_id"], "Actor start response.run_id")
      return Object.freeze({
        session: createSessionRef(sessionID, transport),
        run: Object.freeze({ id: runID }),
      })
    },
  })
}

function definitionQuery(queryInput: ActorListQuery): string {
  const query = new URLSearchParams()
  if (queryInput.deploymentId !== undefined) {
    query.set("deployment_id", resourceID(queryInput.deploymentId, "Deployment ID"))
  }
  if (queryInput.cursor !== undefined) {
    if (queryInput.cursor.length === 0) throw new Error("Actor cursor is required")
    query.set("cursor", queryInput.cursor)
  }
  if (queryInput.limit !== undefined) {
    if (!Number.isInteger(queryInput.limit) || queryInput.limit < 1 || queryInput.limit > 100) {
      throw new Error("Actor limit must be an integer in [1,100]")
    }
    query.set("limit", queryInput.limit.toString())
  }
  return query.size === 0 ? "" : `?${query.toString()}`
}

function parseActorSnapshot(value: unknown): ActorSnapshot {
  const input = objectValue(value, "Actor response")
  const id = requiredString(input, "id", "Actor response")
  validateTaskId(id)
  return Object.freeze({
    id,
    deploymentId: resourceID(input["deployment_id"], "Actor response.deployment_id"),
  })
}

function actorStartBody(request: ClientActorStartRequest): Record<string, unknown> {
  return {
    ...(request.key === undefined ? {} : { key: request.key }),
    ...(request.input === undefined ? {} : { input: request.input }),
    ...(request.idempotencyKey === undefined
      ? {}
      : { idempotency_key: request.idempotencyKey }),
    workspace: { id: requireWorkspaceIDAddress(request.workspace) },
    ...(request.run === undefined ? {} : { run: runOptionsBody(request.run) }),
  }
}

function runOptionsBody(run: NonNullable<ActorStartOptions["run"]>): Record<string, unknown> {
  return {
    ...(run.queue === undefined ? {} : { queue: run.queue }),
    ...(run.concurrencyKey === undefined ? {} : { concurrency_key: run.concurrencyKey }),
    ...(run.priority === undefined ? {} : { priority: run.priority }),
    ...(run.ttl === undefined ? {} : { ttl: run.ttl }),
    ...(run.retry === undefined
      ? {}
      : {
          retry: run.retry.enabled === false
            ? { enabled: false }
            : {
                ...(run.retry.enabled === undefined ? {} : { enabled: run.retry.enabled }),
                max_attempts: run.retry.maxAttempts,
                ...(run.retry.backoff === undefined
                  ? {}
                  : {
                      backoff: {
                        ...(run.retry.backoff.minDelay === undefined
                          ? {}
                          : { min_delay: run.retry.backoff.minDelay }),
                        ...(run.retry.backoff.maxDelay === undefined
                          ? {}
                          : { max_delay: run.retry.backoff.maxDelay }),
                        ...(run.retry.backoff.factor === undefined
                          ? {}
                          : { factor: run.retry.backoff.factor }),
                        ...(run.retry.backoff.jitter === undefined
                          ? {}
                          : { jitter: run.retry.backoff.jitter }),
                      },
                    }),
              },
        }),
    ...(run.metadata === undefined ? {} : { metadata: run.metadata }),
    ...(run.tags === undefined ? {} : { tags: [...run.tags] }),
  }
}

function objectValue(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}

function requiredString(value: Record<string, unknown>, field: string, label: string): string {
  const result = value[field]
  if (typeof result !== "string" || result.length === 0) {
    throw new Error(`${label}.${field} must be a non-empty string`)
  }
  return result
}
