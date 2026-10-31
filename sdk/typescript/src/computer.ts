import { validateSecretName } from "./secret"
import { retentionFromMilliseconds } from "./duration"
import type { SecretBinding } from "./agent"
import type { Duration, Retention, CursorPage } from "./contract"
import type { RequestOptions } from "./request"
import { canonicalSecretOrigin } from "./internal/origin"
import { validateDefinitionId } from "./schema/definition"
import { resourceID } from "./internal/id"
import { timestampString } from "./internal/timestamp"

export type ComputerResidency = "cold" | "starting" | "running" | "parking" | "parked" | "restoring" | "unavailable"

export type ComputerStatus =
  | "available"
  | "deleted"
  | "deleting"

export interface ComputerCreateRequest {
  readonly deleteAfterUnused?: Retention
  readonly template: string
  readonly key?: string
  readonly secrets?: readonly SecretBinding[]
  readonly idempotencyKey?: string
}

export type ComputerExecutionWait = Readonly<{ reason: "scheduling" | "environment_limit" | "saving" }>

interface ComputerProperties {
  readonly executionControl: ComputerExecutionReceipt | null
  readonly executionWait: ComputerExecutionWait | null
  readonly deleteAfterUnused: Retention
  readonly unusedSince: string | null
  readonly id: string
  readonly key?: string
  readonly template: string
  readonly deploymentId: string
  readonly status: ComputerStatus
  readonly secrets: readonly SecretBinding[]
  readonly lastActivityAt: string
  readonly createdAt: string
  readonly updatedAt: string
}

type ComputerError = Readonly<{ code: string; message: string }>
export type Computer = ComputerProperties & (
  | Readonly<{ residency: "unavailable"; error: ComputerError }>
  | Readonly<{ residency: Exclude<ComputerResidency, "unavailable">; error?: ComputerError }>
)

export interface ComputerCommandRequest {
  readonly command: readonly string[]
  readonly cwd?: string
  readonly env?: Readonly<Record<string, string>>
  readonly stdin?: Uint8Array
  readonly timeout?: Duration
  readonly idempotencyKey?: string
}

export interface ComputerDeleteRequest {
  readonly idempotencyKey?: string
  /** Count what deletion would cancel without changing anything. */
  readonly dryRun?: boolean
}

/**
 * Deletion cancels the Computer's Sessions and the Sessions they started, and its
 * unfinished Commands. A dry run reports the same counts and changes nothing.
 */
export interface ComputerDeleteReceipt {
  readonly computerId: string
  readonly sessionsCancelled: number
  readonly commandsCancelled: number
}

export interface ComputerExecutionRequest {
  readonly idempotencyKey?: string
}

/** An accepted request is complete only when status is completed. */
export interface ComputerExecutionReceipt {
  readonly id: string
  readonly computerId: string
  readonly kind: "hibernate" | "wake"
  readonly status: "accepted" | "completed" | "failed"
  readonly errorCode?: string
  readonly acceptedAt: string
  readonly settledAt: string | null
}

export interface ComputerRef {
  hibernate(request?: ComputerExecutionRequest, options?: RequestOptions): Promise<ComputerExecutionReceipt>
  wake(request?: ComputerExecutionRequest, options?: RequestOptions): Promise<ComputerExecutionReceipt>
  update(request: Readonly<{ deleteAfterUnused: Retention }>, options?: RequestOptions): Promise<Computer>
  exec(request: ComputerCommandRequest, options?: RequestOptions): Promise<import("./command").CommandRef>
  readonly id: string
  get(options?: RequestOptions): Promise<Computer>
  readonly sessions: ComputerUsageCollection
  readonly commands: ComputerUsageCollection
  delete(
    request?: ComputerDeleteRequest,
    options?: RequestOptions,
  ): Promise<ComputerDeleteReceipt>
}

export interface ComputerUsageQuery {
  readonly cursor?: string
  readonly limit?: number
}

export interface ComputerUsageCollection {
  list(query?: ComputerUsageQuery, options?: RequestOptions): Promise<CursorPage<ComputerUsage>>
}

export interface ComputerUsage {
  readonly id: string
  readonly usageState: "admitted" | "running" | "waiting" | "parked" | "draining" | "unreconciled"
  readonly createdAt: string
}

export function encodeComputerUsageQuery(query: ComputerUsageQuery): Readonly<{ cursor?: string; limit?: number }> {
  if (query.cursor !== undefined && (typeof query.cursor !== "string" || query.cursor.length === 0)) {
    throw new Error("Computer usage cursor must be a nonempty string")
  }
  if (query.limit !== undefined && (!Number.isInteger(query.limit) || query.limit < 1 || query.limit > 100)) {
    throw new Error("Computer usage limit must be an integer in [1,100]")
  }
  return {
    ...(query.cursor === undefined ? {} : { cursor: query.cursor }),
    ...(query.limit === undefined ? {} : { limit: query.limit }),
  }
}

export function parseComputerUsage(value: unknown, collection: "sessions" | "commands"): CursorPage<ComputerUsage> {
  const response = computerObject(value, "Computer usage response")
  if (!Array.isArray(response[collection])) throw new Error("Computer usage collection must be an array")
  const items = response[collection].map((value): ComputerUsage => {
    const member = computerObject(value, "Computer usage")
    const state = member["usage_state"]
    if (state !== "admitted" && state !== "running" && state !== "waiting" && state !== "parked" && state !== "draining" && state !== "unreconciled") {
      throw new Error("Computer usage.state is invalid")
    }
    return Object.freeze({ id: resourceID(member["id"], "Computer usage.id"), usageState: state,
      createdAt: timestampString(member["created_at"], "Computer usage.created_at") })
  })
  const nextCursor = response["next_cursor"]
  if (nextCursor !== undefined && (typeof nextCursor !== "string" || nextCursor.length === 0)) {
    throw new Error("Computer usage response.next_cursor is invalid")
  }
  return Object.freeze({ items: Object.freeze(items), ...(nextCursor === undefined ? {} : { nextCursor }) })
}

export function encodeComputerSecrets(
  inputs: readonly SecretBinding[] | undefined,
): readonly SecretBinding[] {
  if (inputs === undefined) return Object.freeze([])
  if (!Array.isArray(inputs) || inputs.length > 64) throw new Error("Computer secrets must be an array of at most 64 bindings")
  const envNames = new Set<string>()
  const files: string[] = []
  let originCount = 0
  const result = inputs.map((input): SecretBinding => {
    const value = computerObject(input, "Computer Secret")
    validateSecretName(value["secret"] as string)
    const hasEnv = value["env"] !== undefined
    const hasFile = value["file"] !== undefined
    if (hasEnv === hasFile) throw new Error("Computer Secret requires exactly one of env or file")
    exactBindingKeys(value, ["secret", "env", "file"])
    if (hasEnv) {
      const env = computerObject(value["env"], "Computer Secret env")
      const mode = env["mode"]
      if (mode !== "raw" && mode !== "protected") throw new Error("Computer Secret env requires explicit raw or protected mode")
      exactBindingKeys(env, ["name", "mode", "allowedOrigins"])
      if (mode === "raw" && env["allowedOrigins"] !== undefined) throw new Error("Raw env cannot specify allowedOrigins")
      const name = env["name"]
      if (typeof name !== "string" || !/^[A-Za-z_][A-Za-z0-9_]*$/.test(name) || name.startsWith("HELMR_") || name.startsWith("LD_") || ["NODE_OPTIONS", "NODE_PATH", "NODE_ICU_DATA", "OPENSSL_CONF", "OPENSSL_MODULES", "OPENSSL_ENGINES", "GCONV_PATH", "LOCPATH"].includes(name) || ["SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "NODE_USE_SYSTEM_CA"].includes(name.toUpperCase())) throw new Error("Invalid or reserved Secret env name")
      if (envNames.has(name)) throw new Error(`Duplicate Secret env target ${name}`)
      envNames.add(name)
      if (mode === "raw") return Object.freeze({ secret: input.secret, env: Object.freeze({ name, mode }) })
      const origins = env["allowedOrigins"]
      if (!Array.isArray(origins) || origins.length === 0 || origins.length > 16) throw new Error("Protected env requires 1 to 16 exact HTTPS origins")
      const allowedOrigins = Object.freeze([...new Set(origins.map(canonicalSecretOrigin))].sort())
      originCount += allowedOrigins.length
      return Object.freeze({ secret: input.secret, env: Object.freeze({ name, mode, allowedOrigins }) })
    }
    const file = computerObject(value["file"], "Computer Secret file")
    exactBindingKeys(file, ["path"])
    const path = file["path"]
    if (typeof path !== "string" || path.length > 4096 || new TextEncoder().encode(path).length > 4096 || !path.startsWith("/") || path === "/" || path.includes("\0") || path.split("/").slice(1).some(part => part === "" || part === "." || part === "..") || ["/workspace", "/var/lib/helmr", "/dev", "/opt/helmr", "/proc", "/sys", "/.helmr-old-root", "/run/helmr"].some(root => path === root || path.startsWith(root + "/"))) throw new Error("Invalid or reserved Secret file path")
    files.push(path)
    return Object.freeze({ secret: input.secret, file: Object.freeze({ path }) })
  })
  files.sort()
  if (files.some((path, index) => index > 0 && (path === files[index-1] || path.startsWith(files[index-1] + "/")))) throw new Error("Conflicting Secret file paths")
  if (originCount > 256) throw new Error("Computer Secret origins exceed 256")
  return Object.freeze(result)
}

function exactBindingKeys(value: Record<string, unknown>, allowed: readonly string[]): void {
  const unknown = Object.keys(value).find(key => !allowed.includes(key))
  if (unknown !== undefined) throw new Error(`Computer Secret has unknown member ${JSON.stringify(unknown)}`)
}

export function parseComputer(value: unknown): Computer {
  const input = computerObject(value, "Computer response")
  const key = input["key"]
  if (key !== undefined && typeof key !== "string") {
    throw new Error("Computer response.key must be a string")
  }
  const template = input["template"]
  if (typeof template !== "string") {
    throw new Error("Computer response.template must be a string")
  }
  validateDefinitionId(template)
  const status = input["status"]
  if (
    status !== "available" &&
    status !== "deleted" &&
    status !== "deleting"
  ) {
    throw new Error("Computer response.status is invalid")
  }
  const residency = input["residency"]
  if (typeof residency !== "string" || !["cold", "starting", "running", "parking", "parked", "restoring", "unavailable"].includes(residency)) throw new Error("Computer response.residency is invalid")
  if (residency === "unavailable" && input["error"] === undefined) throw new Error("Unavailable Computer requires an error")
  if (!Array.isArray(input["secrets"])) {
    throw new Error("Computer response.secrets must be an array")
  }
  return Object.freeze({
    id: resourceID(input["id"], "Computer response.id"),
    deleteAfterUnused: retentionFromMilliseconds(input["delete_after_unused_ms"], "deleteAfterUnused"),
    unusedSince: input["unused_since"] === null ? null : timestampString(input["unused_since"], "Computer.unused_since"),
    ...(key === undefined ? {} : { key }),
    template,
    deploymentId: resourceID(input["deployment_id"], "Computer response.deployment_id"),
    status,
    residency: residency as ComputerResidency,
    executionWait: parseExecutionWait(input["execution_wait"]),
    executionControl: input["execution_control"] == null ? null : parseComputerExecutionReceipt(input["execution_control"]),
    ...(input["error"] === undefined ? {} : { error: parseComputerError(input["error"]) }),
    secrets: Object.freeze(input["secrets"].map(parseComputerSecret)),
    lastActivityAt: computerTimestamp(input["last_activity_at"], "last_activity_at"),
    createdAt: computerTimestamp(input["created_at"], "created_at"),
    updatedAt: computerTimestamp(input["updated_at"], "updated_at"),
  } as Computer)
}

function parseComputerSecret(value: unknown): SecretBinding {
  return encodeComputerSecrets([value as SecretBinding])[0]!
}

export function parseComputerDeleteReceipt(
  value: unknown,
): ComputerDeleteReceipt {
  const response = computerObject(value, "Computer delete response")
  const count = (key: string) => {
    const n = response[key]
    if (typeof n !== "number" || !Number.isSafeInteger(n) || n < 0) throw new Error(`Computer delete response.${key} is invalid`)
    return n
  }
  return Object.freeze({
    computerId: resourceID(
      response["computer_id"],
      "Computer delete response.computer_id",
    ),
    sessionsCancelled: count("sessions_cancelled"),
    commandsCancelled: count("commands_cancelled"),
  })
}

function computerObject(
  value: unknown,
  label: string,
): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}

function computerTimestamp(value: unknown, field: string): string {
  return timestampString(value, `Computer response.${field}`)
}

function parseComputerError(value: unknown): NonNullable<Computer["error"]> {
  const input = computerObject(value, "Computer error")
  if (typeof input["code"] !== "string" || typeof input["message"] !== "string") {
    throw new Error("Computer error requires code and message")
  }
  return Object.freeze({ code: input["code"], message: input["message"] })
}

function parseExecutionWait(value: unknown): ComputerExecutionWait | null {
  if (value === null) return null
  const input = computerObject(value, "Computer response.execution_wait")
  const reason = input["reason"]
  if (reason !== "scheduling" && reason !== "environment_limit" && reason !== "saving") {
    throw new Error("Computer response.execution_wait.reason is invalid")
  }
  return Object.freeze({ reason })
}

export function parseComputerExecutionReceipt(value: unknown): ComputerExecutionReceipt {
  const input = computerObject(value, "Computer execution receipt")
  const kind = input["kind"]
  const status = input["status"]
  if (kind !== "hibernate" && kind !== "wake") throw new Error("Invalid Computer execution kind")
  if (status !== "accepted" && status !== "completed" && status !== "failed") throw new Error("Invalid Computer execution status")
  const code = input["error_code"]
  if (code !== undefined && typeof code !== "string") throw new Error("Invalid Computer execution error code")
  return Object.freeze({
    id: resourceID(input["id"], "Computer execution ID"),
    computerId: resourceID(input["computer_id"], "Computer ID"), kind, status,
    ...(code === undefined ? {} : { errorCode: code }),
    acceptedAt: timestampString(input["accepted_at"], "Computer execution accepted_at"),
    settledAt: input["settled_at"] === null ? null : timestampString(input["settled_at"], "Computer execution settled_at"),
  })
}
