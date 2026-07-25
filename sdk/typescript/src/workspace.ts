import type {
  CursorPage,
  Duration,
  WorkspaceIdTarget,
  WorkspaceKeyTarget,
} from "./contract"
import {
  inspectImage,
  type ImageBuilder,
  type InternalImage,
} from "./image"
import type { RequestOptions } from "./request"
import { validateTaskId } from "./schema/task"

const workspaceDefinitionBrand = Symbol.for("helmr.sdk.v0.workspace")

export type WorkspaceMemory = `${bigint}MiB` | `${bigint}GiB`

export interface WorkspaceResources {
  readonly cpu: number
  readonly memory: WorkspaceMemory
}

export type WorkspaceNetwork =
  | Readonly<{
      internet?: true
      denyCidrs?: readonly string[]
    }>
  | Readonly<{
      internet: false
      denyCidrs?: never
    }>

export interface WorkspaceBuilder {
  readonly id: string
  image(value: ImageBuilder): WorkspaceResourceBuilder
}

export interface WorkspaceResourceBuilder {
  readonly id: string
  resources(value: WorkspaceResources): WorkspaceDefinition
}

export type WorkspaceStatus =
  | "available"
  | "recovery-required"
  | "deleting"

export type WorkspaceSecretPlacement =
  | Readonly<{ env: string; file?: never }>
  | Readonly<{ env?: never; file: string }>

export type WorkspaceSecret = Readonly<{ name: string }> &
  WorkspaceSecretPlacement

export interface WorkspaceCreateRequest {
  readonly key?: string
  readonly secrets?: readonly WorkspaceSecret[]
  readonly idempotencyKey?: string
}

export interface RuntimeWorkspaceCreateOptions extends WorkspaceCreateRequest {
  readonly signal?: AbortSignal
}

export interface WorkspaceSnapshot {
  readonly id: string
  readonly key?: string
  readonly declaredId: string
  readonly status: WorkspaceStatus
  readonly secrets: readonly WorkspaceSecret[]
  readonly lastActivityAt: Date
  readonly createdAt: Date
  readonly updatedAt: Date
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

export interface WorkspaceRefBase {
  readonly files: WorkspaceFiles
  retrieve(options?: RequestOptions): Promise<WorkspaceSnapshot>
  exec(
    request: WorkspaceExecRequest,
    options?: RequestOptions,
  ): Promise<WorkspaceExecResult>
  delete(
    request?: WorkspaceDeleteRequest,
    options?: RequestOptions,
  ): Promise<WorkspaceDeleteReceipt>
}

export type WorkspaceIdRef = WorkspaceIdTarget & WorkspaceRefBase
export type WorkspaceKeyRef = WorkspaceKeyTarget & WorkspaceRefBase
export type WorkspaceRef = WorkspaceIdRef | WorkspaceKeyRef

export interface WorkspaceDefinition {
  readonly id: string
  network(value: WorkspaceNetwork): WorkspaceDefinition
  create(options?: RuntimeWorkspaceCreateOptions): Promise<WorkspaceIdRef>
}

export interface Workspaces {
  ref(address: {
    readonly id: string
    readonly key?: never
  }): WorkspaceIdRef
  ref(address: {
    readonly key: string
    readonly id?: never
  }): WorkspaceKeyRef
}

export interface ClientWorkspacesApi {
  create<TWorkspace extends WorkspaceDefinition>(
    workspaceDeclaredId: string,
    request?: WorkspaceCreateRequest,
    options?: RequestOptions,
  ): Promise<WorkspaceIdRef>
  ref<TWorkspace extends WorkspaceDefinition>(
    workspaceDeclaredId: string,
    address: Readonly<{ id: string; key?: never }>,
  ): WorkspaceIdRef
  ref<TWorkspace extends WorkspaceDefinition>(
    workspaceDeclaredId: string,
    address: Readonly<{ key: string; id?: never }>,
  ): WorkspaceKeyRef
}

interface WorkspaceTransport {
  request(
    method: "GET" | "POST",
    path: string,
    options?: Readonly<{ body?: unknown; signal?: AbortSignal }>,
  ): Promise<unknown>
}

export interface InternalWorkspaceDefinition {
  readonly kind: "workspace"
  readonly id: string
  readonly image: InternalImage
  readonly resources: WorkspaceResources
  readonly network?: WorkspaceNetwork
}

class Builder implements WorkspaceBuilder {
  readonly id: string
  constructor(id: string) {
    validateTaskId(id)
    this.id = id
    Object.freeze(this)
  }

  image(value: ImageBuilder): WorkspaceResourceBuilder {
    const inspected = inspectImage(value)
    if (inspected === undefined) {
      throw new Error("workspace.image() requires an image() value")
    }
    return new ResourceBuilder(this.id, inspected)
  }
}

class ResourceBuilder implements WorkspaceResourceBuilder {
  readonly id: string
  readonly imageValue: InternalImage

  constructor(id: string, imageValue: InternalImage) {
    this.id = id
    this.imageValue = imageValue
    Object.freeze(this)
  }

  resources(value: WorkspaceResources): WorkspaceDefinition {
    assertResourceMembers(value)
    return new Definition(
      this.id,
      this.imageValue,
      Object.freeze({ cpu: value.cpu, memory: value.memory }),
    )
  }
}

class Definition implements WorkspaceDefinition {
  readonly id: string
  readonly internal: InternalWorkspaceDefinition

  constructor(
    id: string,
    imageValue: InternalImage,
    resources: WorkspaceResources,
    network?: WorkspaceNetwork,
  ) {
    this.id = id
    this.internal = Object.freeze({
      kind: "workspace" as const,
      id,
      image: imageValue,
      resources,
      ...(network === undefined ? {} : { network }),
    })
    Object.defineProperty(this, workspaceDefinitionBrand, { value: true })
    Object.freeze(this)
  }

  network(value: WorkspaceNetwork): WorkspaceDefinition {
    const network: WorkspaceNetwork =
      value.internet === false
        ? Object.freeze({ internet: false })
        : Object.freeze({
            ...(value.internet === undefined ? {} : { internet: true as const }),
            ...(value.denyCidrs === undefined
              ? {}
              : { denyCidrs: Object.freeze([...value.denyCidrs]) }),
          })
    return new Definition(
      this.id,
      this.internal.image,
      this.internal.resources,
      network,
    )
  }

  create(
    _options?: RuntimeWorkspaceCreateOptions,
  ): Promise<WorkspaceIdRef> {
    return runtimeUnavailable("workspace.create")
  }
}

export function workspace(id: string): WorkspaceBuilder {
  return new Builder(id)
}

function workspaceRef(address: {
  readonly id: string
  readonly key?: never
}): WorkspaceIdRef
function workspaceRef(address: {
  readonly key: string
  readonly id?: never
}): WorkspaceKeyRef
function workspaceRef(
  address:
    | { readonly id: string; readonly key?: never }
    | { readonly key: string; readonly id?: never },
): WorkspaceIdRef | WorkspaceKeyRef {
  if (
    ("id" in address && typeof address.id === "string") ===
    ("key" in address && typeof address.key === "string")
  ) {
    throw new Error("workspace ref requires exactly one of id or key")
  }
  return createWorkspaceRef(address)
}

export const workspaces: Workspaces = Object.freeze({
  ref: workspaceRef,
})

export function createClientWorkspaces(
  transport: WorkspaceTransport,
): ClientWorkspacesApi {
  function ref<TWorkspace extends WorkspaceDefinition>(
    workspaceDeclaredId: string,
    address:
      | Readonly<{ id: string; key?: never }>
      | Readonly<{ key: string; id?: never }>,
  ): WorkspaceIdRef | WorkspaceKeyRef {
    validateTaskId(workspaceDeclaredId)
    validateWorkspaceAddress(address)
    return createAuthenticatedWorkspaceRef(workspaceDeclaredId, address, transport)
  }
  return Object.freeze({
    async create<TWorkspace extends WorkspaceDefinition>(
      workspaceDeclaredId: string,
      request: WorkspaceCreateRequest = {},
      options: RequestOptions = {},
    ): Promise<WorkspaceIdRef> {
      validateTaskId(workspaceDeclaredId)
      const response = workspaceObject(
        await transport.request(
          "POST",
          `/api/workspaces/${encodeURIComponent(workspaceDeclaredId)}/create`,
          {
            body: {
              ...(request.key === undefined ? {} : { key: request.key }),
              ...(request.secrets === undefined
                ? {}
                : {
                    secrets: request.secrets.map((secret) => ({
                      name: secret.name,
                      ...("env" in secret
                        ? { env: secret.env }
                        : { file: secret.file }),
                    })),
                  }),
              ...(request.idempotencyKey === undefined
                ? {}
                : { idempotency_key: request.idempotencyKey }),
            },
            ...(options.signal === undefined ? {} : { signal: options.signal }),
          },
        ),
        "Workspace create response",
      )
      const id = workspacePublicID(
        response["workspace_id"],
        "Workspace create response.workspace_id",
      )
      return createAuthenticatedWorkspaceRef(
        workspaceDeclaredId,
        { id },
        transport,
      ) as WorkspaceIdRef
    },
    ref,
  }) as ClientWorkspacesApi
}

export function inspectWorkspaceDefinition(
  value: unknown,
): InternalWorkspaceDefinition | undefined {
  if (typeof value !== "object" || value === null) return undefined
  if (!Object.hasOwn(value, workspaceDefinitionBrand)) return undefined
  if (
    (value as Record<PropertyKey, unknown>)[workspaceDefinitionBrand] !== true
  ) {
    throw new Error("invalid private workspace record")
  }
  const internal = (value as Partial<Definition>).internal
  if (
    typeof internal !== "object" ||
    internal === null ||
    internal.kind !== "workspace" ||
    typeof internal.id !== "string" ||
    typeof internal.image !== "object" ||
    internal.image === null ||
    typeof internal.resources !== "object" ||
    internal.resources === null
  ) {
    throw new Error("invalid private workspace record")
  }
  validateTaskId(internal.id)
  return internal
}

function createWorkspaceRef(
  address:
    | { readonly id: string; readonly key?: never }
    | { readonly key: string; readonly id?: never },
): WorkspaceIdRef | WorkspaceKeyRef {
  const files: WorkspaceFiles = Object.freeze({
    read(
      _path: string,
      _options?: RequestOptions,
    ): Promise<Uint8Array> {
      return runtimeUnavailable("workspace.files.read")
    },
    stat(
      _path: string,
      _options?: RequestOptions,
    ): Promise<WorkspaceFileEntry> {
      return runtimeUnavailable("workspace.files.stat")
    },
    list(
      _path: string,
      _query?: WorkspaceFileListQuery,
      _options?: RequestOptions,
    ): Promise<CursorPage<WorkspaceFileEntry>> {
      return runtimeUnavailable("workspace.files.list")
    },
  })
  const operations: WorkspaceRefBase = {
    files,
    retrieve(_options) {
      return runtimeUnavailable("workspace.retrieve")
    },
    exec(_options) {
      return runtimeUnavailable("workspace.exec")
    },
    delete(_options) {
      return runtimeUnavailable("workspace.delete")
    },
  }
  return Object.freeze({ ...address, ...operations }) as
    | WorkspaceIdRef
    | WorkspaceKeyRef
}

function createAuthenticatedWorkspaceRef(
  declaredID: string,
  address:
    | Readonly<{ id: string; key?: never }>
    | Readonly<{ key: string; id?: never }>,
  transport: WorkspaceTransport,
): WorkspaceIdRef | WorkspaceKeyRef {
  const resolvePath = (): string => {
    if ("id" in address && address.id !== undefined) {
      return `/api/workspaces/${encodeURIComponent(workspacePublicID(address.id, "Workspace ID"))}`
    }
    return `/api/workspaces/by-key/${encodeURIComponent(declaredID)}?${
      new URLSearchParams({ key: address.key }).toString()
    }`
  }
  const retrieve = async (
    options: RequestOptions = {},
  ): Promise<WorkspaceSnapshot> =>
    parseWorkspaceSnapshot(
      await transport.request(
        "GET",
        resolvePath(),
        options.signal === undefined ? {} : { signal: options.signal },
      ),
    )
  const resolveID = async (signal?: AbortSignal): Promise<string> => {
    if ("id" in address && address.id !== undefined) {
      return workspacePublicID(address.id, "Workspace ID")
    }
    return (await retrieve(signal === undefined ? {} : { signal })).id
  }
  const files: WorkspaceFiles = Object.freeze({
    async read(
      path: string,
      options: RequestOptions = {},
    ): Promise<Uint8Array> {
      const id = await resolveID(options.signal)
      const response = workspaceObject(
        await transport.request(
          "GET",
          `/api/workspaces/${encodeURIComponent(id)}/files/content?${
            new URLSearchParams({ path }).toString()
          }`,
          options.signal === undefined ? {} : { signal: options.signal },
        ),
        "Workspace file response",
      )
      return decodeWorkspaceBase64(
        response["data_base64"],
        "Workspace file response.data_base64",
      )
    },
    async stat(
      path: string,
      options: RequestOptions = {},
    ): Promise<WorkspaceFileEntry> {
      const id = await resolveID(options.signal)
      return parseWorkspaceFileEntry(
        await transport.request(
          "GET",
          `/api/workspaces/${encodeURIComponent(id)}/files/stat?${
            new URLSearchParams({ path }).toString()
          }`,
          options.signal === undefined ? {} : { signal: options.signal },
        ),
      )
    },
    async list(
      path: string,
      queryInput: WorkspaceFileListQuery = {},
      options: RequestOptions = {},
    ): Promise<CursorPage<WorkspaceFileEntry>> {
      const id = await resolveID(options.signal)
      const query = new URLSearchParams({ path })
      if (queryInput.cursor !== undefined) query.set("cursor", queryInput.cursor)
      if (queryInput.limit !== undefined) query.set("limit", String(queryInput.limit))
      const response = workspaceObject(
        await transport.request(
          "GET",
          `/api/workspaces/${encodeURIComponent(id)}/files?${query.toString()}`,
          options.signal === undefined ? {} : { signal: options.signal },
        ),
        "Workspace file list response",
      )
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
    },
  })
  return Object.freeze({
    ...address,
    files,
    retrieve,
    async exec(
      request: WorkspaceExecRequest,
      options: RequestOptions = {},
    ): Promise<WorkspaceExecResult> {
      const id = await resolveID(options.signal)
      const response = workspaceObject(
        await transport.request(
          "POST",
          `/api/workspaces/${encodeURIComponent(id)}/exec`,
          {
            body: {
              command: [...request.command],
              ...(request.cwd === undefined ? {} : { cwd: request.cwd }),
              ...(request.env === undefined ? {} : { env: request.env }),
              ...(request.stdin === undefined
                ? {}
                : { stdin_base64: encodeWorkspaceBase64(request.stdin) }),
              ...(request.timeout === undefined
                ? {}
                : { timeout: request.timeout }),
              idempotency_key: request.idempotencyKey,
            },
            ...(options.signal === undefined ? {} : { signal: options.signal }),
          },
        ),
        "Workspace exec response",
      )
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
    },
    async delete(
      request: WorkspaceDeleteRequest = {},
      options: RequestOptions = {},
    ): Promise<WorkspaceDeleteReceipt> {
      const id = await resolveID(options.signal)
      const response = workspaceObject(
        await transport.request(
          "POST",
          `/api/workspaces/${encodeURIComponent(id)}/delete`,
          {
            body: request.idempotencyKey === undefined
              ? {}
              : { idempotency_key: request.idempotencyKey },
            ...(options.signal === undefined ? {} : { signal: options.signal }),
          },
        ),
        "Workspace delete response",
      )
      return Object.freeze({
        workspaceId: workspacePublicID(
          response["workspace_id"],
          "Workspace delete response.workspace_id",
        ),
      })
    },
  }) as WorkspaceIdRef | WorkspaceKeyRef
}

function validateWorkspaceAddress(
  address:
    | Readonly<{ id: string; key?: never }>
    | Readonly<{ key: string; id?: never }>,
): void {
  if (
    ("id" in address && typeof address.id === "string") ===
    ("key" in address && typeof address.key === "string")
  ) {
    throw new Error("Workspace ref requires exactly one of id or key")
  }
  if ("id" in address && address.id !== undefined) {
    workspacePublicID(address.id, "Workspace ID")
  } else if (address.key.length === 0) {
    throw new Error("Workspace key is required")
  }
}

function parseWorkspaceSnapshot(value: unknown): WorkspaceSnapshot {
  const input = workspaceObject(value, "Workspace response")
  const key = input["key"]
  if (key !== undefined && typeof key !== "string") {
    throw new Error("Workspace response.key must be a string")
  }
  const declaredId = input["declared_id"]
  if (typeof declaredId !== "string") {
    throw new Error("Workspace response.declared_id must be a string")
  }
  validateTaskId(declaredId)
  const status = input["status"]
  if (
    status !== "available" &&
    status !== "recovery-required" &&
    status !== "deleting"
  ) {
    throw new Error("Workspace response.status is invalid")
  }
  if (!Array.isArray(input["secrets"])) {
    throw new Error("Workspace response.secrets must be an array")
  }
  return Object.freeze({
    id: workspacePublicID(input["id"], "Workspace response.id"),
    ...(key === undefined ? {} : { key }),
    declaredId,
    status,
    secrets: Object.freeze(input["secrets"].map(parseWorkspaceSecret)),
    lastActivityAt: workspaceDate(input["last_activity_at"], "last_activity_at"),
    createdAt: workspaceDate(input["created_at"], "created_at"),
    updatedAt: workspaceDate(input["updated_at"], "updated_at"),
  })
}

function parseWorkspaceSecret(value: unknown): WorkspaceSecret {
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

function parseWorkspaceFileEntry(value: unknown): WorkspaceFileEntry {
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

function workspaceObject(
  value: unknown,
  label: string,
): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}

function workspacePublicID(value: unknown, label: string): string {
  if (typeof value !== "string" || !/^wsp_[a-z2-7]{26}$/.test(value)) {
    throw new Error(`${label} must be a canonical Workspace public ID`)
  }
  return value
}

function workspaceDate(value: unknown, field: string): Date {
  if (typeof value !== "string") {
    throw new Error(`Workspace response.${field} must be an RFC 3339 timestamp`)
  }
  const date = new Date(value)
  if (Number.isNaN(date.valueOf())) {
    throw new Error(`Workspace response.${field} must be an RFC 3339 timestamp`)
  }
  return date
}

function encodeWorkspaceBase64(value: Uint8Array): string {
  let binary = ""
  const chunkSize = 32_768
  for (let offset = 0; offset < value.length; offset += chunkSize) {
    binary += String.fromCharCode(...value.subarray(offset, offset + chunkSize))
  }
  return btoa(binary)
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

function runtimeUnavailable<T>(operation: string): T {
  throw new Error(
    `${operation} is unavailable without the Helmr managed runtime or authenticated client`,
  )
}
