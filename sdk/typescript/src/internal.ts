import {
  assertPayloadSchema,
  parsePayloadWithSchema,
  type PayloadSchema,
  type PayloadSchemaInput,
  type PayloadSchemaOutput,
} from "./schema/payload"
import { validateOptionalMaxDurationSeconds, validateTaskId } from "./schema/task"
import type { RunHandle } from "./runtime/run"

export interface CacheMount {
  readonly id: string
}

export type CacheBuilder = CacheMount

export interface CacheMountBinding {
  readonly mountPath: string
  readonly cache: CacheMount
}

export interface SecretMountBinding {
  readonly mountPath: string
  readonly secret: string
}

export interface SourceDirectoryOptions {
  readonly ignore?: readonly string[]
}

export interface WorkspaceSpec {
  readonly kind: "github"
  readonly repository: string
  readonly ref: string
  readonly subpath?: string
}

export type GitHubRefKind = "branch" | "tag" | "sha" | "pull_request" | "unknown"

export interface GitHubPullRequestMetadata {
  readonly number: number
  readonly baseRef: string
  readonly baseSha: string
  readonly headRef: string
  readonly headSha: string
}

export interface GitHubTaskSource {
  readonly kind: "github"
  readonly repository: string
  readonly requestedRef: string
  readonly resolvedSha: string
  readonly refKind?: GitHubRefKind
  readonly refName?: string
  readonly fullRef?: string
  readonly subpath?: string
  readonly defaultBranch?: string
  readonly pullRequest?: GitHubPullRequestMetadata
}

export type TaskSource = GitHubTaskSource

export interface TaskWorkspace {
  readonly path: string
  readonly projectPath: string
}

export interface SourceFileRef {
  readonly path: string
}

export interface SourceDirRef {
  readonly path: string
  readonly ignore: readonly string[]
}

export interface SourceCapabilities {
  file(path: string): SourceFileRef
  directory(path: string, opts?: SourceDirectoryOptions): SourceDirRef
}

export interface WorkspaceCapabilities {
  github(
    repo: string,
    opts: {
      readonly ref: string
      readonly subpath?: string
    },
  ): WorkspaceSpec
}

const approvalTimeoutErrorBrand = Symbol.for("helmr.sdk.ApprovalTimeoutError")
const messageTimeoutErrorBrand = Symbol.for("helmr.sdk.MessageTimeoutError")
const concurrentWaitErrorBrand = Symbol.for("helmr.sdk.ConcurrentWaitError")

export class ApprovalTimeoutError extends Error {
  constructor(message: string) {
    super(message)
    this.name = "ApprovalTimeoutError"
    Object.defineProperty(this, approvalTimeoutErrorBrand, { value: true })
  }

  static override [Symbol.hasInstance](value: unknown): boolean {
    return (
      this === ApprovalTimeoutError &&
      typeof value === "object" &&
      value !== null &&
      approvalTimeoutErrorBrand in value
    )
  }
}

export class MessageTimeoutError extends Error {
  constructor(message: string) {
    super(message)
    this.name = "MessageTimeoutError"
    Object.defineProperty(this, messageTimeoutErrorBrand, { value: true })
  }

  static override [Symbol.hasInstance](value: unknown): boolean {
    return (
      this === MessageTimeoutError &&
      typeof value === "object" &&
      value !== null &&
      messageTimeoutErrorBrand in value
    )
  }
}

export class ConcurrentWaitError extends Error {
  constructor(message: string) {
    super(message)
    this.name = "ConcurrentWaitError"
    Object.defineProperty(this, concurrentWaitErrorBrand, { value: true })
  }

  static override [Symbol.hasInstance](value: unknown): boolean {
    return (
      this === ConcurrentWaitError &&
      typeof value === "object" &&
      value !== null &&
      concurrentWaitErrorBrand in value
    )
  }
}

export type Placement =
  | { readonly env: string }
  | {
      readonly file: string
      readonly mode?: string
      readonly owner?: string
    }
  | {
      readonly dir: string
      readonly mode?: string
      readonly owner?: string
    }

export type SecretDecls = Record<string, Placement>

export function validateSecretName(name: string, label = "secret name"): void {
  if (name.length === 0) {
    throw new Error(`${label} must not be empty`)
  }
  if (name.length > 128) {
    throw new Error(`${label} must be at most 128 characters`)
  }
  if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(name)) {
    throw new Error(`${label} must match /^[A-Za-z_][A-Za-z0-9_]*$/`)
  }
  const upper = name.toUpperCase()
  if (
    upper === "CON" ||
    upper === "PRN" ||
    upper === "AUX" ||
    upper === "NUL" ||
    /^COM[1-9]$/.test(upper) ||
    /^LPT[1-9]$/.test(upper)
  ) {
    throw new Error(`${label} is reserved`)
  }
}

function validateImageBuildSecretRef(name: string): string {
  validateSecretName(name, "image build secret ref")
  return name
}

const RESERVED_WORKSPACE_MOUNT_PATHS = [
  "/dev",
  "/opt/helmr",
  "/proc",
  "/run",
  "/sys",
  "/tmp",
  "/.helmr-old-root",
] as const

function normalizeWorkspaceMountPath(raw: string): string {
  if (raw.length === 0) {
    throw new Error("sandbox.workspace() mountPath is empty")
  }
  if (raw.includes("\0")) {
    throw new Error("sandbox.workspace() mountPath contains NUL")
  }
  if (!raw.startsWith("/")) {
    throw new Error(`sandbox.workspace() mountPath must be absolute: ${raw}`)
  }
  const parts: string[] = []
  for (const part of raw.split("/")) {
    if (part === "" || part === ".") continue
    if (part === "..") {
      throw new Error(`sandbox.workspace() mountPath contains unsafe path components: ${raw}`)
    }
    parts.push(part)
  }
  if (parts.length === 0) {
    throw new Error("sandbox.workspace() mountPath cannot be /")
  }
  const normalized = `/${parts.join("/")}`
  if (
    RESERVED_WORKSPACE_MOUNT_PATHS.some(
      (reserved) => normalized === reserved || normalized.startsWith(`${reserved}/`),
    )
  ) {
    throw new Error(`sandbox.workspace() mountPath conflicts with reserved runtime mount paths: ${normalized}`)
  }
  return normalized
}

export interface TaskContext {
  readonly wait: {
    approval(
      message: string,
      opts?: { readonly timeout?: number; readonly policy?: string },
    ): Promise<{
      readonly approved: boolean
      readonly approvedBy: string
      readonly at: Date
    }>
    message(
      prompt?: string,
      opts?: { readonly timeout?: number; readonly policy?: string },
    ): Promise<{
      readonly text: string
      readonly sentBy: string
      readonly at: Date
      readonly attachments: readonly unknown[]
    }>
  }
  emit(event: EmitEvent): void
  readonly log: {
    info(...args: unknown[]): void
    warn(...args: unknown[]): void
    error(...args: unknown[]): void
  }
  readonly signal: AbortSignal
  readonly run: { readonly id: string }
  readonly task: { readonly id: string }
  readonly source: TaskSource
  readonly workspace: TaskWorkspace
}

export type EmitContent =
  | { readonly type: "text"; readonly text: string }
  | { readonly type: "image"; readonly data: string; readonly mimeType?: string }
  | { readonly type: "tool_use"; readonly name: string; readonly input?: unknown }
  | { readonly type: "tool_result"; readonly name?: string; readonly result?: unknown }
  | { readonly type: string; readonly [key: string]: unknown }

export interface EmitEvent {
  readonly type: string
  readonly content: readonly EmitContent[]
}

export type MaybePromise<T> = T | Promise<T>

declare const noPayloadBrand: unique symbol

export interface NoPayload {
  readonly [noPayloadBrand]: "NoPayload"
}

export interface TaskConfigBase<
  TSecrets extends SecretDecls = Record<never, never>,
> {
  readonly id: string
  readonly sandbox: SandboxBuilder
  readonly maxDuration?: number
  readonly secrets?: TSecrets
}

export type TaskRunOptions<TSecrets extends SecretDecls> = {
  readonly workspace: WorkspaceSpec
  readonly deploymentId?: string
  readonly version?: string
} & ([keyof TSecrets] extends [never]
  ? { readonly secrets?: Record<never, never> }
  : { readonly secrets: { readonly [K in keyof TSecrets]: string } })

export type TaskDirectTrigger<
  TPayloadInput,
  TOutput,
  TSecrets extends SecretDecls,
> = [TPayloadInput] extends [NoPayload]
  ? (opts: TaskRunOptions<TSecrets>) => Promise<RunHandle<Awaited<TOutput>>>
  : (payload: TPayloadInput, opts: TaskRunOptions<TSecrets>) => Promise<RunHandle<Awaited<TOutput>>>

export type TaskConfigWithPayload<
  TPayloadSchema extends PayloadSchema<any, any>,
  TOutput = unknown,
  TSecrets extends SecretDecls = Record<never, never>,
> = TaskConfigBase<TSecrets> & {
  readonly payloadSchema: TPayloadSchema
  readonly run: (payload: PayloadSchemaOutput<TPayloadSchema>, ctx: TaskContext) => MaybePromise<TOutput>
}

export type TaskConfigWithoutPayload<
  TOutput = unknown,
  TSecrets extends SecretDecls = Record<never, never>,
> = TaskConfigBase<TSecrets> & {
  readonly payloadSchema?: undefined
  readonly run: (ctx: TaskContext) => MaybePromise<TOutput>
}

export type TaskConfig<
  TPayload = NoPayload,
  TOutput = unknown,
  TSecrets extends SecretDecls = Record<never, never>,
  TPayloadSchema extends PayloadSchema<any, any> | undefined = undefined,
> =
  TPayloadSchema extends PayloadSchema<any, any>
    ? TaskConfigWithPayload<TPayloadSchema, TOutput, TSecrets>
    : TaskConfigWithoutPayload<TOutput, TSecrets>

export type Task<
  TPayload = NoPayload,
  TOutput = unknown,
  TSecrets extends SecretDecls = Record<never, never>,
  TPayloadInput = TPayload,
> = TaskConfigBase<TSecrets> & {
  readonly "~types"?: {
    readonly payloadInput: TPayloadInput
    readonly payload: TPayload
    readonly output: TOutput
    readonly secrets: TSecrets
  }
  readonly payloadSchema?: PayloadSchema<TPayloadInput, TPayload>
  readonly run: [TPayloadInput] extends [NoPayload]
    ? (ctx: TaskContext) => MaybePromise<TOutput>
    : (payload: TPayload, ctx: TaskContext) => MaybePromise<TOutput>
  readonly trigger: TaskDirectTrigger<TPayloadInput, TOutput, TSecrets>
}
export type AnyTask = TaskConfigBase<SecretDecls> & {
  readonly "~types"?: {
    readonly payloadInput: any
    readonly payload: any
    readonly output: any
    readonly secrets: SecretDecls
  }
  readonly payloadSchema?: PayloadSchema<any, any>
  readonly run: (...args: any[]) => MaybePromise<any>
  readonly trigger: (...args: any[]) => Promise<RunHandle<any>>
}

export type TaskPayload<TTask> =
  TTask extends { readonly "~types"?: { readonly payload: infer TPayload } }
    ? [TPayload] extends [NoPayload] ? never : TPayload
    : never
export type TaskTriggerPayload<TTask> =
  TTask extends { readonly "~types"?: { readonly payloadInput: infer TPayloadInput } } ? TPayloadInput : never
export type TaskOutput<TTask> =
  TTask extends { readonly "~types"?: { readonly output: infer TOutput } } ? Awaited<TOutput> : never
export type TaskSecrets<TTask> =
  TTask extends { readonly "~types"?: { readonly secrets: infer TSecrets } } ? TSecrets : never

export const taskBrand = Symbol.for("helmr.sdk.Task")
export const taskOriginBrand = Symbol.for("helmr.sdk.TaskOrigin")
export const configBrand = Symbol.for("helmr.sdk.Config")

export type ImageCopyInput = SourceFileRef | SourceDirRef | ImageBuilder

export interface ImageRunOptions {
  readonly cache?: readonly CacheMountBinding[]
  readonly secrets?: readonly SecretMountBinding[]
}

export interface ImageBuilder {
  from(ref: string): ImageBuilder
  run(argv: readonly string[], opts?: ImageRunOptions): ImageBuilder
  copy(dest: string, src: ImageCopyInput): ImageBuilder
  copyFrom(dest: string, src: ImageBuilder, srcPath: string): ImageBuilder
  workdir(path: string): ImageBuilder
  env(key: string, value: string): ImageBuilder
  user(name: string): ImageBuilder
}

export interface SandboxBuilder {
  image(img: ImageBuilder): SandboxBuilder
  workspace(mountPath?: string): SandboxBuilder
  resources(opts: { readonly cpu?: number; readonly memory?: string }): SandboxBuilder
}

export type ImageBuildStep =
  | { readonly kind: "from"; readonly ref: string }
  | {
      readonly kind: "run"
      readonly argv: readonly string[]
      readonly cache: readonly CacheMountBinding[]
      readonly secrets: readonly SecretMountBinding[]
    }
  | { readonly kind: "copy"; readonly dest: string; readonly source: ImageCopyInput }
  | { readonly kind: "copyFrom"; readonly dest: string; readonly source: ImageBuilder; readonly srcPath: string }
  | { readonly kind: "workdir"; readonly path: string }
  | { readonly kind: "env"; readonly key: string; readonly value: string }
  | { readonly kind: "user"; readonly name: string }

export interface SandboxWorkspace {
  readonly mountPath: string
}

export interface SandboxResources {
  readonly cpu?: number
  readonly memory?: string
}

const imageBuilderBrand = Symbol.for("helmr.sdk.ImageBuilder")
const sandboxBuilderBrand = Symbol.for("helmr.sdk.SandboxBuilder")
const sourceFileRefBrand = Symbol.for("helmr.sdk.SourceFileRef")
const sourceDirRefBrand = Symbol.for("helmr.sdk.SourceDirRef")

export type MarkedTask<
  TPayload,
  TOutput,
  TSecrets extends SecretDecls,
  TPayloadSchema extends PayloadSchema<any, any> | undefined,
> = TPayloadSchema extends PayloadSchema<any, any>
  ? Task<PayloadSchemaOutput<TPayloadSchema>, Awaited<TOutput>, TSecrets, PayloadSchemaInput<TPayloadSchema>>
  : Task<NoPayload, Awaited<TOutput>, TSecrets, NoPayload>

export function markTask<
  TPayload,
  TOutput,
  TSecrets extends SecretDecls,
  TPayloadSchema extends PayloadSchema<any, any> | undefined,
>(
  config: TaskConfig<TPayload, TOutput, TSecrets, TPayloadSchema>,
): MarkedTask<TPayload, TOutput, TSecrets, TPayloadSchema> {
  validateTaskId(config.id)
  validateOptionalMaxDurationSeconds(config.maxDuration)
  assertPayloadSchema(config.payloadSchema, `task ${JSON.stringify(config.id)} payloadSchema`)
  Object.defineProperty(config, taskBrand, { value: true })
  Object.defineProperty(config, taskOriginBrand, { value: captureTaskOrigin() })
  return config as unknown as MarkedTask<TPayload, TOutput, TSecrets, TPayloadSchema>
}

export async function parseTaskPayload<TTask extends AnyTask>(
  task: TTask,
  payload: unknown,
): Promise<TaskPayload<TTask>> {
  if (task.payloadSchema === undefined) {
    throw new Error(`task ${JSON.stringify(task.id)} does not accept payload`)
  }
  return await parsePayloadWithSchema(task.payloadSchema, payload, `task ${JSON.stringify(task.id)} payload`)
}

export function isTaskDefinition(
  value: unknown,
): value is AnyTask & { readonly [taskBrand]: true } {
  return hasBrand(value, taskBrand)
}

export function taskOriginFile(value: unknown): string | null {
  if (!isTaskDefinition(value)) {
    return null
  }
  const origin = (value as unknown as Record<PropertyKey, unknown>)[taskOriginBrand]
  return typeof origin === "string" && origin.length > 0 ? origin : null
}

export interface HelmrConfig {
  readonly project: string
  readonly dirs: readonly string[]
  readonly ignorePatterns?: readonly string[]
}

export function markConfig(config: HelmrConfig): HelmrConfig {
  Object.defineProperty(config, configBrand, { value: true })
  return config
}

export function isConfigDefinition(value: unknown): value is HelmrConfig & { readonly [configBrand]: true } {
  return hasBrand(value, configBrand)
}

function captureTaskOrigin(): string {
  const stack = new Error().stack ?? ""
  for (const line of stack.split("\n").slice(1)) {
    const file = stackFrameFile(line)
    if (file === null || isSdkInternalFrame(file)) {
      continue
    }
    return file
  }
  return "unknown"
}

function stackFrameFile(line: string): string | null {
  const match = /\(?((?:file:\/\/)?\/[^():]+):\d+:\d+\)?$/.exec(line.trim())
  if (!match?.[1]) {
    return null
  }
  return match[1].startsWith("file://") ? decodeURIComponent(new URL(match[1]).pathname) : match[1]
}

function isSdkInternalFrame(file: string): boolean {
  return (
    file.includes("/sdk/typescript/src/internal") ||
    file.includes("/sdk/typescript/src/task") ||
    file.includes("/sdk/typescript/src/index") ||
    file.includes("/runtime/typescript/src/")
  )
}

export class ImageBuilderImpl implements ImageBuilder {
  readonly id: string
  readonly steps: readonly ImageBuildStep[]

  constructor(id: string, steps: readonly ImageBuildStep[] = []) {
    Object.defineProperty(this, imageBuilderBrand, { value: true })
    this.id = id
    this.steps = steps
  }

  from(ref: string): ImageBuilder {
    return new ImageBuilderImpl(this.id, [...this.steps, { kind: "from", ref }])
  }

  run(argv: readonly string[], opts: ImageRunOptions = {}): ImageBuilder {
    return new ImageBuilderImpl(this.id, [
      ...this.steps,
      {
        kind: "run",
        argv: [...argv],
        cache: (opts.cache ?? []).map((binding) => ({
          mountPath: binding.mountPath,
          cache: { id: binding.cache.id },
        })),
        secrets: (opts.secrets ?? []).map((binding) => ({
          mountPath: binding.mountPath,
          secret: validateImageBuildSecretRef(binding.secret),
        })),
      },
    ])
  }

  copy(dest: string, src: ImageCopyInput): ImageBuilder {
    return new ImageBuilderImpl(this.id, [...this.steps, { kind: "copy", dest, source: src }])
  }

  copyFrom(dest: string, src: ImageBuilder, srcPath: string): ImageBuilder {
    if (!isImageBuilder(src)) {
      throw new Error("image.copyFrom() requires an ImageBuilder created by image()")
    }
    return new ImageBuilderImpl(this.id, [
      ...this.steps,
      { kind: "copyFrom", dest, source: src, srcPath },
    ])
  }

  workdir(path: string): ImageBuilder {
    return new ImageBuilderImpl(this.id, [...this.steps, { kind: "workdir", path }])
  }

  env(key: string, value: string): ImageBuilder {
    return new ImageBuilderImpl(this.id, [...this.steps, { kind: "env", key, value }])
  }

  user(name: string): ImageBuilder {
    return new ImageBuilderImpl(this.id, [...this.steps, { kind: "user", name }])
  }

}

export class SandboxBuilderImpl implements SandboxBuilder {
  readonly id: string
  readonly imageBuilder: ImageBuilderImpl | undefined
  readonly workspaceBinding: SandboxWorkspace | undefined
  readonly resourceSpec: SandboxResources | undefined

  constructor(
    id: string,
    imageBuilder?: ImageBuilderImpl,
    workspaceBinding?: SandboxWorkspace,
    resourceSpec?: SandboxResources,
  ) {
    Object.defineProperty(this, sandboxBuilderBrand, { value: true })
    this.id = id
    this.imageBuilder = imageBuilder
    this.workspaceBinding = workspaceBinding
    this.resourceSpec = resourceSpec
  }

  image(img: ImageBuilder): SandboxBuilder {
    if (!isImageBuilder(img)) {
      throw new Error("sandbox.image() requires an ImageBuilder created by image()")
    }
    return new SandboxBuilderImpl(this.id, img, this.workspaceBinding, this.resourceSpec)
  }

  workspace(mountPath = "/workspace"): SandboxBuilder {
    return new SandboxBuilderImpl(
      this.id,
      this.imageBuilder,
      { mountPath: normalizeWorkspaceMountPath(mountPath) },
      this.resourceSpec,
    )
  }

  resources(opts: { readonly cpu?: number; readonly memory?: string }): SandboxBuilder {
    const resourceSpec: SandboxResources = {
      ...(opts.cpu === undefined ? {} : { cpu: opts.cpu }),
      ...(opts.memory === undefined ? {} : { memory: opts.memory }),
    }
    return new SandboxBuilderImpl(this.id, this.imageBuilder, this.workspaceBinding, resourceSpec)
  }
}

export class SourceFileRefImpl implements SourceFileRef {
  readonly path: string

  constructor(path: string) {
    Object.defineProperty(this, sourceFileRefBrand, { value: true })
    this.path = path
  }
}

export class SourceDirRefImpl implements SourceDirRef {
  readonly path: string
  readonly ignore: readonly string[]

  constructor(path: string, ignore: readonly string[]) {
    Object.defineProperty(this, sourceDirRefBrand, { value: true })
    this.path = path
    this.ignore = ignore
  }
}

export function isImageBuilder(value: unknown): value is ImageBuilderImpl {
  return hasBrand(value, imageBuilderBrand)
}

export function isSandboxBuilder(value: unknown): value is SandboxBuilderImpl {
  return hasBrand(value, sandboxBuilderBrand)
}

export function isSourceFileRef(value: unknown): value is SourceFileRefImpl {
  return hasBrand(value, sourceFileRefBrand)
}

export function isSourceDirRef(value: unknown): value is SourceDirRefImpl {
  return hasBrand(value, sourceDirRefBrand)
}

function hasBrand(value: unknown, brand: symbol): boolean {
  return value !== null && typeof value === "object" && (value as Record<symbol, unknown>)[brand] === true
}
