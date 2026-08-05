import type { JsonValue } from "./contract"
import type { RequestOptions } from "./request"
import { resourceID } from "./internal/id"

export type DeploymentStatus = "queued" | "building" | "deployed" | "failed"

export interface DeploymentFailure {
  readonly code: string
  readonly message: string
  readonly details: Readonly<Record<string, JsonValue>>
}

export interface DeploymentSnapshot {
  readonly id: string
  readonly version: string
  readonly projectId: string
  readonly environmentId: string
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

export interface ClientDeploymentsApi {
  current(options?: RequestOptions): Promise<DeploymentSnapshot | null>
  retrieve(
    deploymentId: string,
    options?: RequestOptions,
  ): Promise<DeploymentSnapshot>
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
    async current(
      options: RequestOptions = {},
    ): Promise<DeploymentSnapshot | null> {
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
    ): Promise<DeploymentSnapshot> {
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

function errorCode(value: unknown): string | undefined {
  if (value === null || typeof value !== "object") return undefined
  const code = (value as { code?: unknown }).code
  return typeof code === "string" ? code : undefined
}

function parseDeployment(value: unknown): DeploymentSnapshot {
  const input = deploymentObject(value, "Deployment response")
  const status = requiredString(input, "status")
  if (status !== "queued" && status !== "building" && status !== "deployed" && status !== "failed") {
    throw new Error("Deployment response.status is invalid")
  }
  const source = deploymentObject(input["deployment_source"], "Deployment response.deployment_source")
  const sizeBytes = optionalPositiveInteger(source, "size_bytes", "Deployment response.deployment_source")
  const failure = input["failure"] === undefined
    ? undefined
    : parseDeploymentFailure(input["failure"])
  if ((status === "failed") !== (failure !== undefined)) {
    throw new Error("Deployment response.failure is inconsistent with status")
  }
  const snapshot: DeploymentSnapshot = {
    id: resourceID(input["id"], "Deployment response.id"),
    version: requiredString(input, "version"),
    projectId: resourceID(input["project_id"], "Deployment response.project_id"),
    environmentId: resourceID(input["environment_id"], "Deployment response.environment_id"),
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
    createdAt: timestamp(input, "created_at"),
    ...optionalTimestamp(input, "building_at", "buildingAt"),
    ...optionalTimestamp(input, "built_at", "builtAt"),
    ...optionalTimestamp(input, "deployed_at", "deployedAt"),
    ...optionalTimestamp(input, "failed_at", "failedAt"),
  }
  return Object.freeze(snapshot)
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

function timestamp(value: Record<string, unknown>, field: string): string {
  const result = requiredString(value, field)
  if (Number.isNaN(Date.parse(result))) {
    throw new Error(`Deployment response.${field} must be a timestamp`)
  }
  return result
}

function optionalTimestamp(
  value: Record<string, unknown>,
  field: string,
  key: string,
): Readonly<Record<string, string>> {
  return value[field] === undefined ? {} : { [key]: timestamp(value, field) }
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
