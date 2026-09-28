import type { Duration, ComputerAddress, CursorPage } from "./contract"
import {
  inspectImage,
  type ImageBuilder,
  type InternalImage,
} from "./image"
import type { RequestOptions } from "./request"
import { validateSecretName } from "./secret"
import { canonicalSecretOrigin } from "./internal/origin"
import { validateTaskId } from "./schema/task"
import { resourceID } from "./internal/id"
import { timestampString } from "./internal/timestamp"
import { currentRuntimeOperations } from "./internal/runtime"

const sandboxDefinitionBrand = Symbol.for("helmr.sdk.v0.sandbox")
const computerAddressBrand = Symbol.for("helmr.sdk.v0.computer-address")

declare const sandboxTypeBrand: unique symbol

export type ComputerMemory = `${bigint}MiB` | `${bigint}GiB`

export interface ComputerResources {
  readonly cpu: number
  readonly memory: ComputerMemory
}

export interface SandboxConfig {
  readonly id: string
}

export interface SandboxBuilder {
  readonly id: string
  image(value: ImageBuilder): SandboxResourceBuilder
}

export interface SandboxResourceBuilder {
  readonly id: string
  resources(value: ComputerResources): Sandbox
}

export type ComputerResidency = "cold" | "starting" | "running" | "parking" | "parked" | "restoring" | "unavailable"

export type ComputerStatus =
  | "available"
  | "deleted"
  | "deleting"

/** A fixed Computer binding. Protected material rotates per HTTP request;
 * raw values resolve per execution admission and remain unchanged in restored memory.
 */
export type ComputerSecretBinding = Readonly<{ secret: string }> & (
  | Readonly<{ env: Readonly<{ name: string; mode: "raw"; allowedOrigins?: never }>; file?: never }>
  | Readonly<{ env: Readonly<{ name: string; mode: "protected"; allowedOrigins: readonly string[] }>; file?: never }>
  | Readonly<{ file: Readonly<{ path: string }>; env?: never }>
)

export interface ComputerCreateRequest {
  readonly key?: string
  readonly secrets?: readonly ComputerSecretBinding[]
  readonly idempotencyKey?: string
}

export type EncodedComputerSecret = Readonly<{ secret: string }> & (
  | Readonly<{ env: Readonly<{ name: string; mode: "raw" | "protected"; allowed_origins?: readonly string[] }>; file?: never }>
  | Readonly<{ file: Readonly<{ path: string }>; env?: never }>
)

export interface Computer {
  readonly error?: Readonly<{ code: string; message: string }>
  readonly id: string
  readonly key?: string
  readonly sandboxId: string
  readonly deploymentId: string
  readonly status: ComputerStatus
  readonly residency: ComputerResidency
  readonly secrets: readonly ComputerSecretBinding[]
  readonly lastActivityAt: string
  readonly createdAt: string
  readonly updatedAt: string
}

export interface ComputerCommandRequest {
  readonly command: readonly string[]
  readonly cwd?: string
  readonly env?: Readonly<Record<string, string>>
  readonly stdin?: Uint8Array
  readonly timeout?: Duration
  readonly idempotencyKey: string
}

export interface ComputerDeleteRequest {
  readonly idempotencyKey?: string
}

export interface ComputerDeleteReceipt {
  readonly computerId: string
}

export interface ComputerRef extends ComputerAddress {
  retrieve(options?: RequestOptions): Promise<Computer>
  members(query?: ComputerMembersQuery, options?: RequestOptions): Promise<CursorPage<ComputerMember>>
  delete(
    request?: ComputerDeleteRequest,
    options?: RequestOptions,
  ): Promise<ComputerDeleteReceipt>
}

export interface ComputerMembersQuery {
  readonly cursor?: string
  readonly limit?: number
}

export interface ComputerMember {
  readonly kind: "session" | "task" | "command"
  readonly id: string
  readonly runId?: string
  readonly state: "admitted" | "running" | "waiting" | "parked" | "draining" | "unreconciled"
  readonly createdAt: string
}

export function encodeComputerMembersQuery(query: ComputerMembersQuery): Readonly<{ cursor?: string; limit?: number }> {
  if (query.cursor !== undefined && (typeof query.cursor !== "string" || query.cursor.length === 0)) {
    throw new Error("Computer member cursor must be a nonempty string")
  }
  if (query.limit !== undefined && (!Number.isInteger(query.limit) || query.limit < 1 || query.limit > 100)) {
    throw new Error("Computer member limit must be an integer in [1,100]")
  }
  return {
    ...(query.cursor === undefined ? {} : { cursor: query.cursor }),
    ...(query.limit === undefined ? {} : { limit: query.limit }),
  }
}

export function parseComputerMembers(value: unknown): CursorPage<ComputerMember> {
  const response = computerObject(value, "Computer members response")
  if (!Array.isArray(response["members"])) throw new Error("Computer members response.members must be an array")
  const items = response["members"].map((value): ComputerMember => {
    const member = computerObject(value, "Computer member")
    const kind = member["kind"]
    if (kind !== "session" && kind !== "task" && kind !== "command") throw new Error("Computer member.kind is invalid")
    const state = member["state"]
    if (state !== "admitted" && state !== "running" && state !== "waiting" && state !== "parked" && state !== "draining" && state !== "unreconciled") {
      throw new Error("Computer member.state is invalid")
    }
    const runId = member["run_id"] === undefined ? undefined : resourceID(member["run_id"], "Computer member.run_id")
    return Object.freeze({ kind, id: resourceID(member["id"], "Computer member.id"), state,
      ...(runId === undefined ? {} : { runId }),
      createdAt: timestampString(member["created_at"], "Computer member.created_at") })
  })
  const nextCursor = response["next_cursor"]
  if (nextCursor !== undefined && (typeof nextCursor !== "string" || nextCursor.length === 0)) {
    throw new Error("Computer members response.next_cursor is invalid")
  }
  return Object.freeze({ items: Object.freeze(items), ...(nextCursor === undefined ? {} : { nextCursor }) })
}

export interface Sandbox {
  readonly [sandboxTypeBrand]: true
  readonly id: string
  createComputer(
    request?: ComputerCreateRequest,
    options?: RequestOptions,
  ): Promise<ComputerRef>
}

interface Computers {
  ref(id: string): ComputerRef
}

export interface InternalSandboxDefinition {
  readonly kind: "sandbox"
  readonly id: string
  readonly image: InternalImage
  readonly resources: ComputerResources
}

class Builder implements SandboxBuilder {
  readonly id: string
  constructor(id: string) {
    validateTaskId(id)
    this.id = id
    Object.freeze(this)
  }

  image(value: ImageBuilder): SandboxResourceBuilder {
    const inspected = inspectImage(value)
    if (inspected === undefined) {
      throw new Error("sandbox.image() requires an image() value")
    }
    return new ResourceBuilder(this.id, inspected)
  }
}

class ResourceBuilder implements SandboxResourceBuilder {
  readonly id: string
  readonly imageValue: InternalImage

  constructor(id: string, imageValue: InternalImage) {
    this.id = id
    this.imageValue = imageValue
    Object.freeze(this)
  }

  resources(value: ComputerResources): Sandbox {
    assertResourceMembers(value)
    return new Definition(
      this.id,
      this.imageValue,
      Object.freeze({ cpu: value.cpu, memory: value.memory }),
    )
  }
}

class Definition implements Sandbox {
  declare readonly [sandboxTypeBrand]: true
  readonly id: string
  readonly internal: InternalSandboxDefinition

  constructor(
    id: string,
    imageValue: InternalImage,
    resources: ComputerResources,
  ) {
    this.id = id
    this.internal = Object.freeze({
      kind: "sandbox" as const,
      id,
      image: imageValue,
      resources,
    })
    Object.defineProperty(this, sandboxDefinitionBrand, { value: true })
    Object.freeze(this)
  }

  createComputer(
    request?: ComputerCreateRequest,
    options?: RequestOptions,
  ): Promise<ComputerRef> {
    return currentRuntimeOperations().computerCreate(
      this.id,
      request,
      options?.signal,
    )
      .then(({ computerId }) =>
        createComputerRef(computerId)
      )
  }
}

export function sandbox(config: SandboxConfig): SandboxBuilder {
  return new Builder(config.id)
}

export const computers: Computers = Object.freeze({
  ref: createComputerRef,
})

export function inspectComputerAddress(
  value: unknown,
): ComputerAddress | undefined {
  if (
    typeof value !== "object" ||
    value === null ||
    (value as Record<PropertyKey, unknown>)[computerAddressBrand] !== true
  ) {
    return undefined
  }
  const address = value as { readonly id?: unknown }
  if (address.id === undefined) {
    throw new Error("private Computer address is invalid")
  }
  return createComputerAddress(address.id as string)
}

export function computerRefID(value: unknown): string {
  const address = inspectComputerAddress(value)
  if (address === undefined || typeof address.id !== "string") {
    throw new Error("Computer requires a Computer ref")
  }
  return address.id
}

export function encodeComputerSecrets(
  inputs: readonly ComputerSecretBinding[] | undefined,
): readonly EncodedComputerSecret[] {
  if (inputs === undefined) return Object.freeze([])
  if (!Array.isArray(inputs) || inputs.length > 64) throw new Error("Computer secrets must be an array of at most 64 bindings")
  const envNames = new Set<string>()
  const files: string[] = []
  let originCount = 0
  const result = inputs.map((input): EncodedComputerSecret => {
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
      const allowed_origins = Object.freeze([...new Set(origins.map(canonicalSecretOrigin))].sort())
      originCount += allowed_origins.length
      return Object.freeze({ secret: input.secret, env: Object.freeze({ name, mode, allowed_origins }) })
    }
    const file = computerObject(value["file"], "Computer Secret file")
    exactBindingKeys(file, ["path"])
    const path = file["path"]
    if (typeof path !== "string" || path.length > 4096 || !path.startsWith("/") || path === "/" || path.includes("\0") || path.split("/").slice(1).some(part => part === "" || part === "." || part === "..") || ["/computer", "/var/lib/helmr", "/dev", "/opt/helmr", "/proc", "/sys", "/.helmr-old-root", "/run/helmr"].some(root => path === root || path.startsWith(root + "/"))) throw new Error("Invalid or reserved Secret file path")
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

export function inspectSandboxDefinition(
  value: unknown,
): InternalSandboxDefinition | undefined {
  if (typeof value !== "object" || value === null) return undefined
  if (!Object.hasOwn(value, sandboxDefinitionBrand)) return undefined
  if (
    (value as Record<PropertyKey, unknown>)[sandboxDefinitionBrand] !== true
  ) {
    throw new Error("invalid private Sandbox record")
  }
  const internal = (value as Partial<Definition>).internal
  if (
    typeof internal !== "object" ||
    internal === null ||
    internal.kind !== "sandbox" ||
    typeof internal.id !== "string" ||
    typeof internal.image !== "object" ||
    internal.image === null ||
    typeof internal.resources !== "object" ||
    internal.resources === null
  ) {
    throw new Error("invalid private Sandbox record")
  }
  validateTaskId(internal.id)
  return internal
}

export function createComputerRef(id: string): ComputerRef {
  const computerID = resourceID(id, "Computer ID")
  const operations: Omit<ComputerRef, keyof ComputerAddress> = {
    members(query = {}, options) {
      return currentRuntimeOperations().computerMembers(computerID, encodeComputerMembersQuery(query), options?.signal)
    },
    retrieve(options) {
      return currentRuntimeOperations().computerRetrieve(
        computerID, options?.signal,
      )
    },
    delete(request, options) {
      return currentRuntimeOperations().computerDelete(
        computerID, request, options?.signal,
      )
    },
  }
  return brandComputerAddress({ id: computerID, ...operations }) as ComputerRef
}

function createComputerAddress(id: string): ComputerAddress {
  return brandComputerAddress({ id: resourceID(id, "Computer ID") })
}

export function brandComputerAddress<T extends { readonly id: string }>(
  value: T,
): T & ComputerAddress {
  resourceID(value.id, "Computer ID")
  return freezeComputerAddress(value) as T & ComputerAddress
}

function freezeComputerAddress<T extends object>(value: T): T {
  Object.defineProperty(value, computerAddressBrand, { value: true })
  return Object.freeze(value)
}

export function parseComputer(value: unknown): Computer {
  const input = computerObject(value, "Computer response")
  const key = input["key"]
  if (key !== undefined && typeof key !== "string") {
    throw new Error("Computer response.key must be a string")
  }
  const sandboxId = input["sandbox_id"]
  if (typeof sandboxId !== "string") {
    throw new Error("Computer response.sandbox_id must be a string")
  }
  validateTaskId(sandboxId)
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
    ...(key === undefined ? {} : { key }),
    sandboxId,
    deploymentId: resourceID(input["deployment_id"], "Computer response.deployment_id"),
    status,
    residency: residency as ComputerResidency,
    ...(input["error"] === undefined ? {} : { error: parseComputerError(input["error"]) }),
    secrets: Object.freeze(input["secrets"].map(parseComputerSecret)),
    lastActivityAt: computerTimestamp(input["last_activity_at"], "last_activity_at"),
    createdAt: computerTimestamp(input["created_at"], "created_at"),
    updatedAt: computerTimestamp(input["updated_at"], "updated_at"),
  })
}

function parseComputerSecret(value: unknown): ComputerSecretBinding {
  const wire = computerObject(value, "Computer Secret")
  exactBindingKeys(wire, wire["env"] !== undefined ? ["secret", "env"] : ["secret", "file"])
  let binding: ComputerSecretBinding
  if (wire["env"] !== undefined) {
    const env = computerObject(wire["env"], "Computer Secret env")
    exactBindingKeys(env, ["name", "mode", "allowed_origins"])
    binding = { secret: wire["secret"], env: { name: env["name"], mode: env["mode"], ...(env["allowed_origins"] === undefined ? {} : { allowedOrigins: env["allowed_origins"] }) } } as ComputerSecretBinding
  } else {
    binding = { secret: wire["secret"], file: wire["file"] } as ComputerSecretBinding
  }
  const normalized = encodeComputerSecrets([binding])[0]!
  return normalized.env !== undefined
    ? Object.freeze({ secret: normalized.secret, env: Object.freeze({ name: normalized.env.name, mode: normalized.env.mode, ...(normalized.env.allowed_origins === undefined ? {} : { allowedOrigins: normalized.env.allowed_origins }) }) }) as ComputerSecretBinding
    : Object.freeze({ secret: normalized.secret, file: normalized.file })
}

export function parseComputerDeleteReceipt(
  value: unknown,
): ComputerDeleteReceipt {
  const response = computerObject(value, "Computer delete response")
  return Object.freeze({
    computerId: resourceID(
      response["computer_id"],
      "Computer delete response.computer_id",
    ),
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

function assertResourceMembers(value: ComputerResources): void {
  if (
    value === null ||
    typeof value !== "object" ||
    Array.isArray(value) ||
    !Object.hasOwn(value, "cpu") ||
    !Object.hasOwn(value, "memory")
  ) {
    throw new Error("computer resources require cpu and memory")
  }
  const members = Object.keys(value)
  if (
    members.length !== 2 ||
    !members.includes("cpu") ||
    !members.includes("memory")
  ) {
    throw new Error("computer resources support only cpu and memory")
  }
}

export function parseComputerError(value: unknown): NonNullable<Computer["error"]> {
  const input = computerObject(value, "Computer error")
  if (typeof input["code"] !== "string" || typeof input["message"] !== "string") {
    throw new Error("Computer error requires code and message")
  }
  return Object.freeze({ code: input["code"], message: input["message"] })
}
