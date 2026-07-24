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
import { validateTaskId } from "./schema/task"

const workspaceDefinitionBrand = Symbol.for("helmr.sdk.v0.workspace")

export interface WorkspaceResources {
  readonly cpu: number
  readonly memory: string
  readonly disk: string
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

export interface WorkspaceCreateOptions {
  readonly key?: string
  readonly secrets?: readonly WorkspaceSecret[]
  readonly idempotencyKey?: string
  readonly signal?: AbortSignal
}

export interface WorkspaceRetrieveOptions {
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

export type WorkspaceFileOptions = WorkspaceRetrieveOptions

export interface WorkspaceFileListOptions extends WorkspaceRetrieveOptions {
  readonly cursor?: string
  readonly limit?: number
}

export interface WorkspaceFiles {
  read(path: string, options?: WorkspaceFileOptions): Promise<Uint8Array>
  stat(
    path: string,
    options?: WorkspaceFileOptions,
  ): Promise<WorkspaceFileEntry>
  list(
    path: string,
    options?: WorkspaceFileListOptions,
  ): Promise<CursorPage<WorkspaceFileEntry>>
}

export interface WorkspaceExecOptions {
  readonly command: readonly string[]
  readonly cwd?: string
  readonly env?: Readonly<Record<string, string>>
  readonly stdin?: Uint8Array
  readonly timeout?: Duration
  readonly idempotencyKey: string
  readonly signal?: AbortSignal
}

export interface WorkspaceExecResult {
  readonly exitCode: number
  readonly stdout: Uint8Array
  readonly stderr: Uint8Array
}

export interface WorkspaceDeleteOptions extends WorkspaceRetrieveOptions {
  readonly idempotencyKey?: string
}

export interface WorkspaceDeleteReceipt {
  readonly workspaceId: string
}

export interface WorkspaceRefBase {
  readonly files: WorkspaceFiles
  retrieve(options?: WorkspaceRetrieveOptions): Promise<WorkspaceSnapshot>
  exec(options: WorkspaceExecOptions): Promise<WorkspaceExecResult>
  delete(options?: WorkspaceDeleteOptions): Promise<WorkspaceDeleteReceipt>
}

export type WorkspaceIdRef = WorkspaceIdTarget & WorkspaceRefBase
export type WorkspaceKeyRef = WorkspaceKeyTarget & WorkspaceRefBase
export type WorkspaceRef = WorkspaceIdRef | WorkspaceKeyRef

export interface WorkspaceDefinition {
  readonly id: string
  network(value: WorkspaceNetwork): WorkspaceDefinition
  create(options?: WorkspaceCreateOptions): Promise<WorkspaceIdRef>
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
    return new Definition(this.id, this.imageValue, Object.freeze({ ...value }))
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
    _options?: WorkspaceCreateOptions,
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
      _options?: WorkspaceFileOptions,
    ): Promise<Uint8Array> {
      return runtimeUnavailable("workspace.files.read")
    },
    stat(
      _path: string,
      _options?: WorkspaceFileOptions,
    ): Promise<WorkspaceFileEntry> {
      return runtimeUnavailable("workspace.files.stat")
    },
    list(
      _path: string,
      _options?: WorkspaceFileListOptions,
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

function assertResourceMembers(value: WorkspaceResources): void {
  if (
    value === null ||
    typeof value !== "object" ||
    !Object.hasOwn(value, "cpu") ||
    !Object.hasOwn(value, "memory") ||
    !Object.hasOwn(value, "disk")
  ) {
    throw new Error(
      "workspace resources require cpu, memory, and disk",
    )
  }
}

function runtimeUnavailable<T>(operation: string): T {
  throw new Error(
    `${operation} is unavailable without the Helmr managed runtime or authenticated client`,
  )
}
