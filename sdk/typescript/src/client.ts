import { createClientAgents, type ClientAgentsApi } from "./client-agent"
import type { CommandRef } from "./command"
import type { JsonValue } from "./contract"
import {
  createClientComputers,
  createClientCommandRef,
  type ClientComputersApi,
} from "./client-computer"
import {
  createClientComputerDefinitions,
  type ClientComputerDefinitionsApi,
} from "./client-computer-definition"
import {
  createClientSessions,
  type ClientSessionsApi,
} from "./client-session"
import {
  createClientDeployments,
  type ClientDeploymentsApi,
} from "./client-deployment"
import {
  createClientSchedules,
  type ClientSchedulesApi,
} from "./client-schedule"
import {
  createClientSecrets,
  type ClientSecretsApi,
} from "./client-secret"

export interface HelmrClientOptions {
  readonly url?: string
  readonly apiKey: string
  readonly fetch?: typeof fetch
}

export class HelmrClient {
  readonly agents: ClientAgentsApi
  readonly sessions: ClientSessionsApi
  readonly computerDefinitions: ClientComputerDefinitionsApi
  readonly deployments: ClientDeploymentsApi
  readonly schedules: ClientSchedulesApi
  readonly secrets: ClientSecretsApi
  readonly commands: Readonly<{ ref(id: string): CommandRef }>
  readonly computers: ClientComputersApi

  constructor(options: HelmrClientOptions) {
    const transport = new ClientTransport(options)
    this.agents = createClientAgents(transport)
    this.sessions = createClientSessions(transport)
    this.computerDefinitions = createClientComputerDefinitions(transport)
    this.deployments = createClientDeployments(transport)
    this.schedules = createClientSchedules(transport)
    this.secrets = createClientSecrets(transport)
    this.computers = createClientComputers(transport)
    this.commands = Object.freeze({ ref: (id: string) => createClientCommandRef(id, transport) })
  }
}

class ClientTransport {
  readonly #baseURL: URL
  readonly #apiKey: string
  readonly #fetch: typeof fetch

  constructor(options: HelmrClientOptions) {
    this.#baseURL = clientBaseURL(options.url ?? "https://api.helmr.dev")
    this.#apiKey = options.apiKey.trim()
    if (this.#apiKey === "") throw new Error("Helmr API key is required")
    this.#fetch = options.fetch ?? globalThis.fetch
    if (typeof this.#fetch !== "function") {
      throw new Error("fetch is unavailable")
    }
  }

  async request(
    method: "GET" | "POST" | "DELETE",
    path: string,
    options: Readonly<{ body?: unknown; signal?: AbortSignal }> = {},
  ): Promise<unknown> {
    const response = await this.#fetch(new URL(path, this.#baseURL), {
      method,
      headers: {
        Authorization: `Bearer ${this.#apiKey}`,
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
  const payload = body["error"] !== null && typeof body["error"] === "object" &&
      !Array.isArray(body["error"])
    ? body["error"] as Record<string, unknown>
    : {}
  const message = typeof payload["message"] === "string" && payload["message"] !== ""
    ? payload["message"]
    : `Helmr request failed with status ${response.status}`
  const code = typeof payload["code"] === "string" && payload["code"] !== ""
    ? payload["code"]
    : responseStatusCode(response.status)
  const details = payload["details"] !== null && typeof payload["details"] === "object" &&
      !Array.isArray(payload["details"])
    ? payload["details"] as Readonly<Record<string, JsonValue>>
    : undefined
  const requestId = response.headers.get("X-Request-ID")?.trim() || undefined
  const error = new Error(message) as Error & {
    code: string
    details?: Readonly<Record<string, JsonValue>>
    requestId?: string
  }
  error.name = "HelmrError"
  error.code = code
  if (details !== undefined) error.details = details
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
    case 405:
      return "method_not_allowed"
    case 409:
      return "conflict"
    case 410:
      return "gone"
    case 413:
      return "request_too_large"
    case 422:
      return "unprocessable_entity"
    case 429:
      return "rate_limited"
    case 501:
      return "not_implemented"
    case 502:
      return "bad_gateway"
    case 503:
      return "service_unavailable"
    default:
      return status >= 500 ? "internal_error" : "request_failed"
  }
}

function clientBaseURL(raw: string): URL {
  const url = new URL(raw)
  if (url.username !== "" || url.password !== "" ||
    (url.pathname !== "" && url.pathname !== "/") || url.search !== "" || url.hash !== "") {
    throw new Error("Helmr API URL must be an origin without credentials, path, query, or fragment")
  }
  if (url.protocol !== "https:" && !(
    url.protocol === "http:" &&
    (url.hostname === "localhost" || url.hostname === "127.0.0.1" || url.hostname === "[::1]")
  )) {
    throw new Error("Helmr API URL must use HTTPS except on loopback")
  }
  url.pathname = "/"
  return url
}
