import type { CursorPage, JsonValue } from "./contract"
import type { RequestOptions } from "./request"
import { resourceID } from "./internal/id"
import { timestampString } from "./internal/timestamp"

export type DeploymentStatus = "queued" | "building" | "deployed" | "failed"

export interface DeploymentFailure {
  readonly code: string
  readonly message: string
  readonly details: Readonly<Record<string, JsonValue>>
}

export interface Deployment {
  readonly id: string
  readonly version: string
  readonly contentHash: string
  readonly deploymentSource: Readonly<{
    digest: string
    sizeBytes?: number
    mediaType?: string
  }>
  readonly status: DeploymentStatus
  readonly failure?: DeploymentFailure
  readonly createdAt: string
  readonly buildingAt?: string
  readonly builtAt?: string
  readonly deployedAt?: string
  readonly failedAt?: string
}

export interface DeploymentListItem {
  readonly id: string
  readonly version: string
  readonly status: DeploymentStatus
  readonly createdAt: string
  readonly buildingAt?: string
  readonly builtAt?: string
  readonly deployedAt?: string
  readonly failedAt?: string
}

export interface DeploymentListQuery {
  readonly cursor?: string
  readonly limit?: number
}

export interface ClientDeploymentsApi {
  list(
    query?: DeploymentListQuery,
    options?: RequestOptions,
  ): Promise<CursorPage<DeploymentListItem>>
  current(options?: RequestOptions): Promise<Deployment | null>
  retrieve(
    deploymentId: string,
    options?: RequestOptions,
  ): Promise<Deployment>
}

interface DeploymentTransport {
  request(
    method: "GET",
    path: string,
    options?: Readonly<{ signal?: AbortSignal }>,
  ): Promise<unknown>
}

export function createClientDeployments(
  transport: DeploymentTransport,
): ClientDeploymentsApi {
  return Object.freeze({
    async list(
      queryInput: DeploymentListQuery = {},
      options: RequestOptions = {},
    ): Promise<CursorPage<DeploymentListItem>> {
      const query = new URLSearchParams()
      if (queryInput.cursor !== undefined) {
        if (queryInput.cursor.length === 0) throw new Error("Deployment cursor is required")
        query.set("cursor", queryInput.cursor)
      }
      if (queryInput.limit !== undefined) {
        if (!Number.isInteger(queryInput.limit) || queryInput.limit < 1 || queryInput.limit > 100) {
          throw new Error("Deployment limit must be an integer in [1,100]")
        }
        query.set("limit", String(queryInput.limit))
      }
      const suffix = query.size === 0 ? "" : `?${query.toString()}`
      const response = deploymentObject(await transport.request(
        "GET",
        `/v1/deployments${suffix}`,
        options.signal === undefined ? {} : { signal: options.signal },
      ), "Deployment list response")
      if (!Array.isArray(response["deployments"])) {
        throw new Error("Deployment list response.deployments must be an array")
      }
      const nextCursor = response["next_cursor"]
      if (nextCursor !== undefined && typeof nextCursor !== "string") {
        throw new Error("Deployment list response.next_cursor must be a string")
      }
      return Object.freeze({
        items: Object.freeze(response["deployments"].map(parseDeploymentListItem)),
        ...(nextCursor === undefined ? {} : { nextCursor }),
      })
    },
    async current(
      options: RequestOptions = {},
    ): Promise<Deployment | null> {
      try {
        return parseDeployment(await transport.request(
          "GET",
          "/v1/deployments/current",
          options.signal === undefined ? {} : { signal: options.signal },
        ))
      } catch (error) {
        if (errorCode(error) === "no_current_deployment") return null
        throw error
      }
    },
    async retrieve(
      deploymentId: string,
      options: RequestOptions = {},
    ): Promise<Deployment> {
      return parseDeployment(
        await transport.request(
          "GET",
          `/v1/deployments/${encodeURIComponent(resourceID(deploymentId, "Deployment ID"))}`,
          options.signal === undefined ? {} : { signal: options.signal },
        ),
      )
    },
  })
}

function parseDeploymentListItem(value: unknown): DeploymentListItem {
  const input = deploymentObject(value, "Deployment list item")
  const status = deploymentStatus(input["status"], "Deployment list item")
  return Object.freeze({
    id: resourceID(input["id"], "Deployment list item.id"),
    version: requiredString(input, "version", "Deployment list item"),
    status,
    createdAt: timestamp(input, "created_at", "Deployment list item"),
    ...optionalTimestamp(input, "building_at", "buildingAt", "Deployment list item"),
    ...optionalTimestamp(input, "built_at", "builtAt", "Deployment list item"),
    ...optionalTimestamp(input, "deployed_at", "deployedAt", "Deployment list item"),
    ...optionalTimestamp(input, "failed_at", "failedAt", "Deployment list item"),
  })
}

function errorCode(value: unknown): string | undefined {
  if (value === null || typeof value !== "object") return undefined
  const code = (value as { code?: unknown }).code
  return typeof code === "string" ? code : undefined
}

function parseDeployment(value: unknown): Deployment {
  const input = deploymentObject(value, "Deployment response")
  const status = deploymentStatus(input["status"], "Deployment response")
  const source = deploymentObject(input["deployment_source"], "Deployment response.deployment_source")
  const sizeBytes = optionalPositiveInteger(source, "size_bytes", "Deployment response.deployment_source")
  const failure = input["failure"] === undefined
    ? undefined
    : parseDeploymentFailure(input["failure"])
  if ((status === "failed") !== (failure !== undefined)) {
    throw new Error("Deployment response.failure is inconsistent with status")
  }
  const deployment: Deployment = {
    id: resourceID(input["id"], "Deployment response.id"),
    version: requiredString(input, "version"),
    contentHash: requiredString(input, "content_hash"),
    deploymentSource: Object.freeze({
      digest: requiredString(source, "digest", "Deployment response.deployment_source"),
      ...(sizeBytes === undefined ? {} : { sizeBytes }),
      ...(source["media_type"] === undefined
        ? {}
        : { mediaType: requiredString(source, "media_type", "Deployment response.deployment_source") }),
    }),
    status,
    ...(failure === undefined ? {} : { failure }),
    createdAt: timestamp(input, "created_at", "Deployment response"),
    ...optionalTimestamp(input, "building_at", "buildingAt"),
    ...optionalTimestamp(input, "built_at", "builtAt"),
    ...optionalTimestamp(input, "deployed_at", "deployedAt"),
    ...optionalTimestamp(input, "failed_at", "failedAt"),
  }
  return Object.freeze(deployment)
}

function parseDeploymentFailure(value: unknown): DeploymentFailure {
  const input = deploymentObject(value, "Deployment response.failure")
  const details = deploymentObject(input["details"], "Deployment response.failure.details")
  return Object.freeze({
    code: requiredString(input, "code", "Deployment response.failure"),
    message: requiredString(input, "message", "Deployment response.failure"),
    details: Object.freeze({ ...details }) as Readonly<Record<string, JsonValue>>,
  })
}

function deploymentObject(
  value: unknown,
  label: string,
): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}

function requiredString(
  value: Record<string, unknown>,
  field: string,
  label = "Deployment response",
): string {
  const result = value[field]
  if (typeof result !== "string" || result === "") {
    throw new Error(`${label}.${field} must be a non-empty string`)
  }
  return result
}

function timestamp(value: Record<string, unknown>, field: string, label: string): string {
  return timestampString(value[field], `${label}.${field}`)
}

function optionalTimestamp(
  value: Record<string, unknown>,
  field: string,
  key: string,
  label = "Deployment response",
): Readonly<Record<string, string>> {
  return value[field] === undefined ? {} : { [key]: timestamp(value, field, label) }
}

function deploymentStatus(value: unknown, label: string): DeploymentStatus {
  if (value !== "queued" && value !== "building" && value !== "deployed" && value !== "failed") {
    throw new Error(`${label}.status is invalid`)
  }
  return value
}

function optionalPositiveInteger(
  value: Record<string, unknown>,
  field: string,
  label: string,
): number | undefined {
  const result = value[field]
  if (result === undefined) return undefined
  if (!Number.isSafeInteger(result) || (result as number) <= 0) {
    throw new Error(`${label}.${field} must be a positive integer`)
  }
  return result as number
}
