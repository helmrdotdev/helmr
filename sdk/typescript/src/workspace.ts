import type {
  CursorPage,
  Duration,
  WorkspaceAddress,
} from "./contract"
import {
  inspectImage,
  type ImageBuilder,
  type InternalImage,
} from "./image"
import type { RequestOptions } from "./request"
import { inspectSecretAddress, type SecretAddress } from "./secret"
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

export type WorkspaceSecretPlacement =
  | Readonly<{ env: string; file?: never }>
  | Readonly<{ env?: never; file: string }>

export type WorkspaceSecretInput = Readonly<{ secret: SecretAddress }> &
  WorkspaceSecretPlacement

export type WorkspaceSecretInfo = Readonly<{ name: string }> &
  WorkspaceSecretPlacement

export interface WorkspaceCreateRequest {
  readonly key?: string
  readonly secrets?: readonly WorkspaceSecretInput[]
  readonly idempotencyKey?: string
}

export type EncodedWorkspaceSecret = Readonly<{ name: string }> &
  WorkspaceSecretPlacement

export interface Workspace {
  readonly id: string
  readonly key?: string
  readonly sandboxId: string
  readonly deploymentId: string
  readonly status: WorkspaceStatus
  readonly secrets: readonly WorkspaceSecretInfo[]
  readonly lastActivityAt: string
  readonly createdAt: string
  readonly updatedAt: string
}

export type WorkspaceFileEntry =
  | Readonly<{
      path: string
      kind: "file"
      mode: number
      sizeBytes: number
    }>
  | Readonly<{
      path: string
      kind: "directory"
      mode: number
    }>
  | Readonly<{
      path: string
      kind: "symlink"
      mode: number
      linkTarget: string
    }>

export interface WorkspaceFileListQuery {
  readonly cursor?: string
  readonly limit?: number
}

export interface WorkspaceFiles {
  read(path: string, options?: RequestOptions): Promise<Uint8Array>
  stat(
    path: string,
    options?: RequestOptions,
  ): Promise<WorkspaceFileEntry>
  list(
    path: string,
    query?: WorkspaceFileListQuery,
    options?: RequestOptions,
  ): Promise<CursorPage<WorkspaceFileEntry>>
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
  readonly files: WorkspaceFiles
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
  inputs: readonly WorkspaceSecretInput[] | undefined,
): readonly EncodedWorkspaceSecret[] {
  if (inputs === undefined) return Object.freeze([])
  if (!Array.isArray(inputs)) {
    throw new Error("Workspace secrets must be an array")
  }
  if (inputs.length > 64) {
    throw new Error("at most 64 Workspace Secret placements are allowed")
  }
  return Object.freeze(inputs.map((input) => {
    if (typeof input !== "object" || input === null || Array.isArray(input)) {
      throw new Error("Workspace Secret placement must be an object")
    }
    if (!Object.hasOwn(input, "secret")) {
      throw new Error("Workspace Secret placement.secret is required")
    }
    const name = inspectSecretAddress(input.secret)
    if (name === undefined) {
      throw new Error("Workspace Secret requires secrets.fromName()")
    }
    const hasEnv = Object.hasOwn(input, "env") && input.env !== undefined
    const hasFile = Object.hasOwn(input, "file") && input.file !== undefined
    if (hasEnv === hasFile) {
      throw new Error("Workspace Secret requires exactly one of env or file")
    }
    const allowed = new Set(hasEnv ? ["secret", "env"] : ["secret", "file"])
    const unknown = Object.keys(input).find((key) => !allowed.has(key))
    if (unknown !== undefined) {
      throw new Error(
        `Workspace Secret placement has unknown member ${JSON.stringify(unknown)}`,
      )
    }
    if (hasEnv) {
      if (typeof input.env !== "string" || input.env.length === 0) {
        throw new Error("Workspace Secret env target is required")
      }
      return Object.freeze({ name, env: input.env })
    }
    if (typeof input.file !== "string" || input.file.length === 0) {
      throw new Error("Workspace Secret file target is required")
    }
    return Object.freeze({ name, file: input.file })
  }))
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
  const files: WorkspaceFiles = Object.freeze({
    read(
      path: string,
      options?: RequestOptions,
    ): Promise<Uint8Array> {
      return currentRuntimeOperations().workspaceFileRead(
        workspaceID, path, options?.signal,
      )
    },
    stat(
      path: string,
      options?: RequestOptions,
    ): Promise<WorkspaceFileEntry> {
      return currentRuntimeOperations().workspaceFileStat(
        workspaceID, path, options?.signal,
      )
    },
    list(
      path: string,
      query?: WorkspaceFileListQuery,
      options?: RequestOptions,
    ): Promise<CursorPage<WorkspaceFileEntry>> {
      return currentRuntimeOperations().workspaceFileList(
        workspaceID, path, query, options?.signal,
      )
    },
  })
  const operations: Omit<WorkspaceRef, keyof WorkspaceAddress> = {
    files,
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
  return Object.freeze({
    id: resourceID(input["id"], "Workspace response.id"),
    ...(key === undefined ? {} : { key }),
    sandboxId,
    deploymentId: resourceID(input["deployment_id"], "Workspace response.deployment_id"),
    status,
    secrets: Object.freeze(input["secrets"].map(parseWorkspaceSecret)),
    lastActivityAt: workspaceTimestamp(input["last_activity_at"], "last_activity_at"),
    createdAt: workspaceTimestamp(input["created_at"], "created_at"),
    updatedAt: workspaceTimestamp(input["updated_at"], "updated_at"),
  })
}

function parseWorkspaceSecret(value: unknown): WorkspaceSecretInfo {
  const input = workspaceObject(value, "Workspace Secret")
  if (typeof input["name"] !== "string") {
    throw new Error("Workspace Secret.name must be a string")
  }
  const hasEnv = typeof input["env"] === "string"
  const hasFile = typeof input["file"] === "string"
  if (hasEnv === hasFile) {
    throw new Error("Workspace Secret must contain exactly one placement")
  }
  return Object.freeze({
    name: input["name"],
    ...(hasEnv ? { env: input["env"] as string } : { file: input["file"] as string }),
  })
}

export function parseWorkspaceFileEntry(value: unknown): WorkspaceFileEntry {
  const input = workspaceObject(value, "Workspace file entry")
  if (
    typeof input["path"] !== "string" ||
    typeof input["mode"] !== "number" ||
    !Number.isInteger(input["mode"])
  ) {
    throw new Error("Workspace file entry identity is invalid")
  }
  switch (input["kind"]) {
    case "file":
      if (typeof input["size_bytes"] !== "number" || !Number.isSafeInteger(input["size_bytes"])) {
        throw new Error("Workspace file entry.size_bytes must be an integer")
      }
      return Object.freeze({
        path: input["path"],
        kind: "file",
        mode: input["mode"],
        sizeBytes: input["size_bytes"],
      })
    case "directory":
      return Object.freeze({
        path: input["path"],
        kind: "directory",
        mode: input["mode"],
      })
    case "symlink":
      if (typeof input["link_target"] !== "string") {
        throw new Error("Workspace symlink entry.link_target must be a string")
      }
      return Object.freeze({
        path: input["path"],
        kind: "symlink",
        mode: input["mode"],
        linkTarget: input["link_target"],
      })
    default:
      throw new Error("Workspace file entry.kind is invalid")
  }
}

export function parseWorkspaceFileContent(value: unknown): Uint8Array {
  const response = workspaceObject(value, "Workspace file response")
  return decodeWorkspaceBase64(
    response["data_base64"],
    "Workspace file response.data_base64",
  )
}

export function parseWorkspaceFilePage(
  value: unknown,
): CursorPage<WorkspaceFileEntry> {
  const response = workspaceObject(value, "Workspace file list response")
  if (!Array.isArray(response["items"])) {
    throw new Error("Workspace file list response.items must be an array")
  }
  const nextCursor = response["next_cursor"]
  if (nextCursor !== undefined && typeof nextCursor !== "string") {
    throw new Error("Workspace file list response.next_cursor must be a string")
  }
  return Object.freeze({
    items: Object.freeze(response["items"].map(parseWorkspaceFileEntry)),
    ...(nextCursor === undefined ? {} : { nextCursor }),
  })
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
