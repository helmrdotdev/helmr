import type { CursorPage } from "./contract"
import type { RequestOptions } from "./request"
import { resourceID } from "./internal/id"
import { timestampString } from "./internal/timestamp"

export interface Deployment {
  readonly id: string
  readonly version: string
  readonly bundleDigest: string
  readonly createdAt: string
}

export interface DeploymentListItem {
  readonly id: string
  readonly version: string
  readonly bundleDigest: string
  readonly createdAt: string
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
  return Object.freeze({
    id: resourceID(input["id"], "Deployment list item.id"),
    version: requiredString(input, "version", "Deployment list item"),
    bundleDigest: requiredString(input, "bundle_digest", "Deployment list item"),
    createdAt: timestamp(input, "created_at", "Deployment list item"),
  })
}

function errorCode(value: unknown): string | undefined {
  if (value === null || typeof value !== "object") return undefined
  const code = (value as { code?: unknown }).code
  return typeof code === "string" ? code : undefined
}

function parseDeployment(value: unknown): Deployment {
  const input = deploymentObject(value, "Deployment response")
  const deployment: Deployment = {
    id: resourceID(input["id"], "Deployment response.id"),
    version: requiredString(input, "version"),
    bundleDigest: requiredString(input, "bundle_digest"),
    createdAt: timestamp(input, "created_at", "Deployment response"),
  }
  return Object.freeze(deployment)
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
