import type {
  ActorStartResult,
  ActorStartOptions,
  CursorPage,
} from "./contract"
import { resourceID } from "./internal/id"
import { createRunHandle } from "./internal/run-handle"
import type { RequestOptions } from "./request"
import { validateTaskId } from "./schema/task"
import { createSessionRef } from "./client-session"
import { workspaceRefID } from "./workspace"
import {
  definitionItemQuery,
  definitionListQuery,
} from "./internal/definition-query"

export type ActorStartRequest = Omit<ActorStartOptions, "signal">

export interface ActorRetrieveQuery {
  readonly deploymentId?: string
}

export interface ActorListQuery extends ActorRetrieveQuery {
  readonly cursor?: string
  readonly limit?: number
}

export interface ActorListItem {
  readonly id: string
}

export interface ActorInfo extends ActorListItem {
  readonly deploymentId: string
}

export interface ActorPage extends CursorPage<ActorListItem> {
  readonly deploymentId: string
}

export interface ClientActorsApi {
  retrieve(
    id: string,
    query?: ActorRetrieveQuery,
    options?: RequestOptions,
  ): Promise<ActorInfo>
  list(
    query?: ActorListQuery,
    options?: RequestOptions,
  ): Promise<ActorPage>
  start(
    id: string,
    request: ActorStartRequest,
    options?: RequestOptions,
  ): Promise<ActorStartResult>
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
      query: ActorRetrieveQuery = {},
      options: RequestOptions = {},
    ): Promise<ActorInfo> {
      validateTaskId(id)
      return parseActorInfo(await transport.request(
        "GET",
        `/v1/actors/${encodeURIComponent(id)}${definitionItemQuery(query, "Actor")}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ))
    },
    async list(
      queryInput: ActorListQuery = {},
      options: RequestOptions = {},
    ): Promise<ActorPage> {
      const response = objectValue(await transport.request(
        "GET",
        `/v1/actors${definitionListQuery(queryInput, "Actor")}`,
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
        items: Object.freeze(response["actors"].map(parseActorListItem)),
        ...(nextCursor === undefined ? {} : { nextCursor }),
      })
    },
    async start(
      id: string,
      request: ActorStartRequest,
      options: RequestOptions = {},
    ): Promise<ActorStartResult> {
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
        run: createRunHandle<null>(runID),
      })
    },
  })
}

function parseActorInfo(value: unknown): ActorInfo {
  const input = objectValue(value, "Actor response")
  const id = requiredString(input, "id", "Actor response")
  validateTaskId(id)
  return Object.freeze({
    id,
    deploymentId: resourceID(input["deployment_id"], "Actor response.deployment_id"),
  })
}

function parseActorListItem(value: unknown): ActorListItem {
  const input = objectValue(value, "Actor list item")
  const id = requiredString(input, "id", "Actor list item")
  validateTaskId(id)
  return Object.freeze({ id })
}

function actorStartBody(request: ActorStartRequest): Record<string, unknown> {
  return {
    ...(request.key === undefined ? {} : { key: request.key }),
    ...(request.input === undefined ? {} : { input: request.input }),
    ...(request.idempotencyKey === undefined
      ? {}
      : { idempotency_key: request.idempotencyKey }),
    workspace: { id: workspaceRefID(request.workspace) },
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
