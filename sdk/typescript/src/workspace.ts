import type { Duration, WorkspaceAddress } from "./contract"
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
const workspaceAddressBrand = Symbol.for("helmr.sdk.v0.workspace-address")

declare const sandboxTypeBrand: unique symbol

export type WorkspaceMemory = `${bigint}MiB` | `${bigint}GiB`

export interface WorkspaceResources {
  readonly cpu: number
  readonly memory: WorkspaceMemory
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
  resources(value: WorkspaceResources): Sandbox
}

export type WorkspaceStatus =
  | "available"
  | "recovery_required"
  | "deleting"

/** A fixed Workspace binding. Protected material rotates per HTTP request;
 * raw values resolve per execution admission and remain unchanged in restored memory.
 */
export type WorkspaceSecretBinding = Readonly<{ secret: string }> & (
  | Readonly<{ env: Readonly<{ name: string; mode: "raw"; allowedOrigins?: never }>; file?: never }>
  | Readonly<{ env: Readonly<{ name: string; mode: "protected"; allowedOrigins: readonly string[] }>; file?: never }>
  | Readonly<{ file: Readonly<{ path: string }>; env?: never }>
)

export interface WorkspaceCreateRequest {
  readonly key?: string
  readonly secrets?: readonly WorkspaceSecretBinding[]
  readonly idempotencyKey?: string
}

export type EncodedWorkspaceSecret = Readonly<{ secret: string }> & (
  | Readonly<{ env: Readonly<{ name: string; mode: "raw" | "protected"; allowed_origins?: readonly string[] }>; file?: never }>
  | Readonly<{ file: Readonly<{ path: string }>; env?: never }>
)

/**
 * The Session or Run that currently owns a Workspace. Absent on an unowned
 * Workspace.
 */
export type WorkspaceOwner =
  | Readonly<{ sessionId: string; runId?: never }>
  | Readonly<{ runId: string; sessionId?: never }>

export interface Workspace {
  readonly id: string
  readonly key?: string
  readonly sandboxId: string
  readonly deploymentId: string
  readonly status: WorkspaceStatus
  readonly owner?: WorkspaceOwner
  readonly secrets: readonly WorkspaceSecretBinding[]
  readonly lastActivityAt: string
  readonly createdAt: string
  readonly updatedAt: string
}

export interface WorkspaceExecRequest {
  readonly command: readonly string[]
  readonly cwd?: string
  readonly env?: Readonly<Record<string, string>>
  readonly stdin?: Uint8Array
  readonly timeout?: Duration
  readonly idempotencyKey: string
}

export interface WorkspaceExecResult {
  readonly exitCode: number
  readonly stdout: Uint8Array
  readonly stderr: Uint8Array
}

export interface WorkspaceDeleteRequest {
  readonly idempotencyKey?: string
}

export interface WorkspaceDeleteReceipt {
  readonly workspaceId: string
}

export interface WorkspaceRef extends WorkspaceAddress {
  retrieve(options?: RequestOptions): Promise<Workspace>
  exec(
    request: WorkspaceExecRequest,
    options?: RequestOptions,
  ): Promise<WorkspaceExecResult>
  delete(
    request?: WorkspaceDeleteRequest,
    options?: RequestOptions,
  ): Promise<WorkspaceDeleteReceipt>
}

export interface Sandbox {
  readonly [sandboxTypeBrand]: true
  readonly id: string
  createWorkspace(
    request?: WorkspaceCreateRequest,
    options?: RequestOptions,
  ): Promise<WorkspaceRef>
}

interface Workspaces {
  ref(id: string): WorkspaceRef
}

export interface InternalSandboxDefinition {
  readonly kind: "sandbox"
  readonly id: string
  readonly image: InternalImage
  readonly resources: WorkspaceResources
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

  resources(value: WorkspaceResources): Sandbox {
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
    resources: WorkspaceResources,
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

  createWorkspace(
    request?: WorkspaceCreateRequest,
    options?: RequestOptions,
  ): Promise<WorkspaceRef> {
    return currentRuntimeOperations().workspaceCreate(
      this.id,
      request,
      options?.signal,
    )
      .then(({ workspaceId }) =>
        createWorkspaceRef(workspaceId)
      )
  }
}

export function sandbox(config: SandboxConfig): SandboxBuilder {
  return new Builder(config.id)
}

export const workspaces: Workspaces = Object.freeze({
  ref: createWorkspaceRef,
})

export function inspectWorkspaceAddress(
  value: unknown,
): WorkspaceAddress | undefined {
  if (
    typeof value !== "object" ||
    value === null ||
    (value as Record<PropertyKey, unknown>)[workspaceAddressBrand] !== true
  ) {
    return undefined
  }
  const address = value as { readonly id?: unknown }
  if (address.id === undefined) {
    throw new Error("private Workspace address is invalid")
  }
  return createWorkspaceAddress(address.id as string)
}

export function workspaceRefID(value: unknown): string {
  const address = inspectWorkspaceAddress(value)
  if (address === undefined || typeof address.id !== "string") {
    throw new Error("Workspace requires a Workspace ref")
  }
  return address.id
}

export function encodeWorkspaceSecrets(
  inputs: readonly WorkspaceSecretBinding[] | undefined,
): readonly EncodedWorkspaceSecret[] {
  if (inputs === undefined) return Object.freeze([])
  if (!Array.isArray(inputs) || inputs.length > 64) throw new Error("Workspace secrets must be an array of at most 64 bindings")
  const envNames = new Set<string>()
  const files: string[] = []
  let originCount = 0
  const result = inputs.map((input): EncodedWorkspaceSecret => {
    const value = workspaceObject(input, "Workspace Secret")
    validateSecretName(value["secret"] as string)
    const hasEnv = value["env"] !== undefined
    const hasFile = value["file"] !== undefined
    if (hasEnv === hasFile) throw new Error("Workspace Secret requires exactly one of env or file")
    exactBindingKeys(value, ["secret", "env", "file"])
    if (hasEnv) {
      const env = workspaceObject(value["env"], "Workspace Secret env")
      const mode = env["mode"]
      if (mode !== "raw" && mode !== "protected") throw new Error("Workspace Secret env requires explicit raw or protected mode")
      exactBindingKeys(env, ["name", "mode", "allowedOrigins"])
      if (mode === "raw" && env["allowedOrigins"] !== undefined) throw new Error("Raw env cannot specify allowedOrigins")
      const name = env["name"]
      if (typeof name !== "string" || !/^[A-Za-z_][A-Za-z0-9_]*$/.test(name) || name.startsWith("HELMR_") || name.startsWith("LD_") || ["NODE_OPTIONS", "NODE_PATH", "NODE_ICU_DATA", "OPENSSL_CONF", "OPENSSL_MODULES", "OPENSSL_ENGINES", "GCONV_PATH", "LOCPATH"].includes(name) || ["HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "NODE_USE_ENV_PROXY", "NODE_USE_SYSTEM_CA"].includes(name.toUpperCase())) throw new Error("Invalid or reserved Secret env name")
      if (envNames.has(name)) throw new Error(`Duplicate Secret env target ${name}`)
      envNames.add(name)
      if (mode === "raw") return Object.freeze({ secret: input.secret, env: Object.freeze({ name, mode }) })
      const origins = env["allowedOrigins"]
      if (!Array.isArray(origins) || origins.length === 0 || origins.length > 16) throw new Error("Protected env requires 1 to 16 exact HTTPS origins")
      const allowed_origins = Object.freeze([...new Set(origins.map(canonicalSecretOrigin))].sort())
      originCount += allowed_origins.length
      return Object.freeze({ secret: input.secret, env: Object.freeze({ name, mode, allowed_origins }) })
    }
    const file = workspaceObject(value["file"], "Workspace Secret file")
    exactBindingKeys(file, ["path"])
    const path = file["path"]
    if (typeof path !== "string" || path.length > 4096 || !path.startsWith("/") || path === "/" || path.includes("\0") || path.split("/").slice(1).some(part => part === "" || part === "." || part === "..") || ["/workspace", "/var/lib/helmr", "/dev", "/opt/helmr", "/proc", "/sys", "/.helmr-old-root", "/run/helmr"].some(root => path === root || path.startsWith(root + "/"))) throw new Error("Invalid or reserved Secret file path")
    files.push(path)
    return Object.freeze({ secret: input.secret, file: Object.freeze({ path }) })
  })
  files.sort()
  if (files.some((path, index) => index > 0 && (path === files[index-1] || path.startsWith(files[index-1] + "/")))) throw new Error("Conflicting Secret file paths")
  if (originCount > 256) throw new Error("Workspace Secret origins exceed 256")
  return Object.freeze(result)
}

function exactBindingKeys(value: Record<string, unknown>, allowed: readonly string[]): void {
  const unknown = Object.keys(value).find(key => !allowed.includes(key))
  if (unknown !== undefined) throw new Error(`Workspace Secret has unknown member ${JSON.stringify(unknown)}`)
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

export function createWorkspaceRef(id: string): WorkspaceRef {
  const workspaceID = resourceID(id, "Workspace ID")
  const operations: Omit<WorkspaceRef, keyof WorkspaceAddress> = {
    retrieve(options) {
      return currentRuntimeOperations().workspaceRetrieve(
        workspaceID, options?.signal,
      )
    },
    exec(request, options) {
      return currentRuntimeOperations().workspaceExec(
        workspaceID, request, options?.signal,
      )
    },
    delete(request, options) {
      return currentRuntimeOperations().workspaceDelete(
        workspaceID, request, options?.signal,
      )
    },
  }
  return brandWorkspaceAddress({ id: workspaceID, ...operations }) as WorkspaceRef
}

function createWorkspaceAddress(id: string): WorkspaceAddress {
  return brandWorkspaceAddress({ id: resourceID(id, "Workspace ID") })
}

export function brandWorkspaceAddress<T extends { readonly id: string }>(
  value: T,
): T & WorkspaceAddress {
  resourceID(value.id, "Workspace ID")
  return freezeWorkspaceAddress(value) as T & WorkspaceAddress
}

function freezeWorkspaceAddress<T extends object>(value: T): T {
  Object.defineProperty(value, workspaceAddressBrand, { value: true })
  return Object.freeze(value)
}

export function parseWorkspace(value: unknown): Workspace {
  const input = workspaceObject(value, "Workspace response")
  const key = input["key"]
  if (key !== undefined && typeof key !== "string") {
    throw new Error("Workspace response.key must be a string")
  }
  const sandboxId = input["sandbox_id"]
  if (typeof sandboxId !== "string") {
    throw new Error("Workspace response.sandbox_id must be a string")
  }
  validateTaskId(sandboxId)
  const status = input["status"]
  if (
    status !== "available" &&
    status !== "recovery_required" &&
    status !== "deleting"
  ) {
    throw new Error("Workspace response.status is invalid")
  }
  if (!Array.isArray(input["secrets"])) {
    throw new Error("Workspace response.secrets must be an array")
  }
  const owner = parseWorkspaceOwner(input["owner"], "Workspace response.owner")
  return Object.freeze({
    id: resourceID(input["id"], "Workspace response.id"),
    ...(key === undefined ? {} : { key }),
    sandboxId,
    deploymentId: resourceID(input["deployment_id"], "Workspace response.deployment_id"),
    status,
    ...(owner === undefined ? {} : { owner }),
    secrets: Object.freeze(input["secrets"].map(parseWorkspaceSecret)),
    lastActivityAt: workspaceTimestamp(input["last_activity_at"], "last_activity_at"),
    createdAt: workspaceTimestamp(input["created_at"], "created_at"),
    updatedAt: workspaceTimestamp(input["updated_at"], "updated_at"),
  })
}

export function parseWorkspaceOwner(value: unknown, label: string): WorkspaceOwner | undefined {
  if (value === undefined) return undefined
  const input = workspaceObject(value, label)
  const hasSession = input["session_id"] !== undefined
  const hasRun = input["run_id"] !== undefined
  if (hasSession === hasRun) {
    throw new Error(`${label} must name exactly one of session_id or run_id`)
  }
  return Object.freeze(
    hasSession
      ? { sessionId: resourceID(input["session_id"], `${label}.session_id`) }
      : { runId: resourceID(input["run_id"], `${label}.run_id`) },
  )
}

function parseWorkspaceSecret(value: unknown): WorkspaceSecretBinding {
  const wire = workspaceObject(value, "Workspace Secret")
  exactBindingKeys(wire, wire["env"] !== undefined ? ["secret", "env"] : ["secret", "file"])
  let binding: WorkspaceSecretBinding
  if (wire["env"] !== undefined) {
    const env = workspaceObject(wire["env"], "Workspace Secret env")
    exactBindingKeys(env, ["name", "mode", "allowed_origins"])
    binding = { secret: wire["secret"], env: { name: env["name"], mode: env["mode"], ...(env["allowed_origins"] === undefined ? {} : { allowedOrigins: env["allowed_origins"] }) } } as WorkspaceSecretBinding
  } else {
    binding = { secret: wire["secret"], file: wire["file"] } as WorkspaceSecretBinding
  }
  const normalized = encodeWorkspaceSecrets([binding])[0]!
  return normalized.env !== undefined
    ? Object.freeze({ secret: normalized.secret, env: Object.freeze({ name: normalized.env.name, mode: normalized.env.mode, ...(normalized.env.allowed_origins === undefined ? {} : { allowedOrigins: normalized.env.allowed_origins }) }) }) as WorkspaceSecretBinding
    : Object.freeze({ secret: normalized.secret, file: normalized.file })
}

export function parseWorkspaceExecResult(
  value: unknown,
): WorkspaceExecResult {
  const response = workspaceObject(value, "Workspace exec response")
  const exitCode = response["exit_code"]
  if (!Number.isSafeInteger(exitCode)) {
    throw new Error("Workspace exec response.exit_code must be an integer")
  }
  return Object.freeze({
    exitCode: exitCode as number,
    stdout: decodeWorkspaceBase64(
      response["stdout_base64"],
      "Workspace exec response.stdout_base64",
    ),
    stderr: decodeWorkspaceBase64(
      response["stderr_base64"],
      "Workspace exec response.stderr_base64",
    ),
  })
}

export function parseWorkspaceDeleteReceipt(
  value: unknown,
): WorkspaceDeleteReceipt {
  const response = workspaceObject(value, "Workspace delete response")
  return Object.freeze({
    workspaceId: resourceID(
      response["workspace_id"],
      "Workspace delete response.workspace_id",
    ),
  })
}

function workspaceObject(
  value: unknown,
  label: string,
): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}

function workspaceTimestamp(value: unknown, field: string): string {
  return timestampString(value, `Workspace response.${field}`)
}

function decodeWorkspaceBase64(value: unknown, label: string): Uint8Array {
  if (typeof value !== "string") {
    throw new Error(`${label} must be canonical padded base64`)
  }
  let binary: string
  try {
    binary = atob(value)
  } catch {
    throw new Error(`${label} must be canonical padded base64`)
  }
  if (btoa(binary) !== value) {
    throw new Error(`${label} must be canonical padded base64`)
  }
  const output = new Uint8Array(binary.length)
  for (let index = 0; index < binary.length; index++) {
    output[index] = binary.charCodeAt(index)
  }
  return output
}

function assertResourceMembers(value: WorkspaceResources): void {
  if (
    value === null ||
    typeof value !== "object" ||
    Array.isArray(value) ||
    !Object.hasOwn(value, "cpu") ||
    !Object.hasOwn(value, "memory")
  ) {
    throw new Error("workspace resources require cpu and memory")
  }
  const members = Object.keys(value)
  if (
    members.length !== 2 ||
    !members.includes("cpu") ||
    !members.includes("memory")
  ) {
    throw new Error("workspace resources support only cpu and memory")
  }
}
