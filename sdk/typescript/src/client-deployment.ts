import type { RequestOptions } from "./request"
import { resourceID } from "./internal/id"

export interface DeploymentSnapshot {
  readonly id: string
  readonly version: string
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
      const response = deploymentObject(
        await transport.request(
          "GET",
          "/v1/deployments/current",
          options.signal === undefined ? {} : { signal: options.signal },
        ),
        "Current Deployment response",
      )
      return response["deployment"] === null ||
          response["deployment"] === undefined
        ? null
        : parseDeployment(response["deployment"])
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

function parseDeployment(value: unknown): DeploymentSnapshot {
  const input = deploymentObject(value, "Deployment response")
  return Object.freeze({
    id: resourceID(input["id"], "Deployment response.id"),
    version: requiredString(input, "version"),
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

function requiredString(value: Record<string, unknown>, field: string): string {
  const result = value[field]
  if (typeof result !== "string" || result === "") {
    throw new Error(`Deployment response.${field} must be a non-empty string`)
  }
  return result
}
