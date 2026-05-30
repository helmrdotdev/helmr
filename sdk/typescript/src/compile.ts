import { create, toBinary } from "@bufbuild/protobuf"
import { createHash } from "node:crypto"
import {
  BundleSchema,
  CacheMountBindingSchema,
  CopyFromImageSchema,
  CopySourceDirSchema,
  CopySourceFileSchema,
  DirPlacementSchema,
  EnvSchema,
  EnvPlacementSchema,
  FilePlacementSchema,
  FromSchema,
  ImageSpecSchema,
  ImageStepSchema,
  PlacementSchema as BundlePlacementSchema,
  PlatformSchema,
  ResourcesSchema,
  RunSchema,
  SandboxSpecSchema,
  SecretPlacementSchema as BundleSecretPlacementSchema,
  SecretMountBindingSchema,
  SecretRefSchema,
  SourceDirRefSchema,
  SourceFileRefSchema,
  TaskSpecSchema,
  UserSchema,
  WorkdirSchema,
  WorkspaceRuntimeBindingSchema,
  type Bundle,
  type ImageSpec,
  type ImageStep,
} from "@helmr/proto"

import {
  ImageBuilderImpl,
  SandboxBuilderImpl,
  isImageBuilder,
  isSandboxBuilder,
  isSourceDirRef,
  isSourceFileRef,
  validateSecretName,
  type AnyTask,
  type ImageCopyInput,
  type ImageBuildStep,
  type Placement,
  type SandboxWorkspace,
} from "./internal"
import { readOptionalMaxDurationSeconds } from "./schema/task"

export interface CompileOptions {
  readonly task: AnyTask
  readonly modulePath: string
  readonly exportName?: string
}

export const IMAGE_FORMAT_VERSION = 0
const IMAGE_KEY_DOMAIN = "helmr.image.v0\n"

export function compile(opts: CompileOptions): Bundle {
  const task = opts.task
  if (!isSandboxBuilder(task.sandbox)) {
    throw new Error(`task "${task.id}" must declare sandbox: sandbox(...)`)
  }

  const compiler = new BundleCompiler()
  const imageSpec = compiler.compileSandboxImage(task.sandbox)
  const subImages = compiler.compileSubImages(imageSpec)
  const workspace = compiler.compileWorkspace(task.sandbox)
  const resources = task.sandbox.resourceSpec
  const maxDurationSeconds = readOptionalMaxDurationSeconds(task.maxDuration, `task "${task.id}" maxDuration`)
  const sandboxSpec = create(SandboxSpecSchema, {
    id: task.sandbox.id,
    workspace,
    ...(resources
      ? {
          resources: create(ResourcesSchema, {
            ...(resources.cpu === undefined ? {} : { cpu: resources.cpu }),
            ...(resources.memory === undefined ? {} : { memory: resources.memory }),
          }),
        }
      : {}),
  })

  return create(BundleSchema, {
    image: imageSpec,
    sandbox: sandboxSpec,
    subImages,
    task: create(TaskSpecSchema, {
      id: task.id,
      sandboxId: task.sandbox.id,
      modulePath: opts.modulePath,
      exportName: opts.exportName ?? "default",
      maxDurationSeconds,
      ...(task.payloadSchema === undefined
        ? {}
        : { payloadSchemaJson: payloadSchemaJson(task.payloadSchema) }),
      secrets: Object.entries(readSecretDecls(task.secrets)).map(([name, placement]) =>
        create(BundleSecretPlacementSchema, {
          name,
          placement: compilePlacement(placement),
        }),
      ),
    }),
  })
}

function payloadSchemaJson(schema: NonNullable<AnyTask["payloadSchema"]>): string {
  const jsonSchema = schema.toJSONSchema({ io: "input", unrepresentable: "any" })
  assertJSONSchemaMetadata(jsonSchema)
  const encoded = JSON.stringify(jsonSchema)
  if (encoded === undefined) {
    throw new Error("payloadSchema.toJSONSchema() must return JSON-serializable metadata")
  }
  return encoded
}

function assertJSONSchemaMetadata(value: unknown): void {
  if (typeof value === "boolean") {
    return
  }
  if (value !== null && typeof value === "object" && !Array.isArray(value)) {
    return
  }
  throw new Error("payloadSchema.toJSONSchema() must return a JSON Schema object or boolean")
}

function compilePlacement(placement: Placement) {
  if ("env" in placement) {
    return create(BundlePlacementSchema, {
      kind: {
        case: "env",
        value: create(EnvPlacementSchema, {
          name: placement.env,
        }),
      },
    })
  }
  if ("file" in placement) {
    return create(BundlePlacementSchema, {
      kind: {
        case: "file",
        value: create(FilePlacementSchema, {
          path: placement.file,
          ...(placement.mode === undefined ? {} : { mode: placement.mode }),
          ...(placement.owner === undefined ? {} : { owner: placement.owner }),
        }),
      },
    })
  }
  return create(BundlePlacementSchema, {
    kind: {
      case: "dir",
      value: create(DirPlacementSchema, {
        path: placement.dir,
        ...(placement.mode === undefined ? {} : { mode: placement.mode }),
        ...(placement.owner === undefined ? {} : { owner: placement.owner }),
      }),
    },
  })
}

class BundleCompiler {
  readonly imageSpecs = new Map<ImageBuilderImpl, ImageSpec>()

  compileSandboxImage(sandbox: SandboxBuilderImpl): ImageSpec {
    const image = sandbox.imageBuilder
    if (!image) {
      throw new Error(`sandbox "${sandbox.id}" must declare image(...)`)
    }
    return this.compileImage(image)
  }

  compileWorkspace(sandbox: SandboxBuilderImpl) {
    const workspace: SandboxWorkspace = sandbox.workspaceBinding ?? {
      mountPath: "/workspace",
    }
    return create(WorkspaceRuntimeBindingSchema, {
      mountPath: workspace.mountPath,
    })
  }

  compileImage(image: ImageBuilderImpl): ImageSpec {
    const existing = this.imageSpecs.get(image)
    if (existing) {
      return existing
    }
    if (image.steps.length === 0) {
      throw new Error(`image "${image.id}" must contain at least one operation`)
    }
    const spec = create(ImageSpecSchema, {
      formatVersion: IMAGE_FORMAT_VERSION,
      platform: create(PlatformSchema, { os: "linux", architecture: currentArchitecture() }),
      steps: image.steps.map((step) => this.compileBuildStep(step)),
    })
    this.imageSpecs.set(image, spec)
    return spec
  }

  compileSubImages(root: ImageSpec): Record<string, ImageSpec> {
    const values: Record<string, ImageSpec> = {}
    for (const spec of this.imageSpecs.values()) {
      if (spec === root) {
        continue
      }
      values[compileProvisionalImageKey(spec)] = spec
    }
    return values
  }

  compileBuildStep(step: ImageBuildStep): ImageStep {
    switch (step.kind) {
      case "from":
        return create(ImageStepSchema, {
          kind: { case: "from", value: create(FromSchema, { ref: step.ref }) },
        })
      case "run":
        return create(ImageStepSchema, {
          kind: {
            case: "run",
            value: create(RunSchema, {
              argv: [...step.argv],
              cacheMounts: step.cache.map((binding) =>
                create(CacheMountBindingSchema, {
                  dst: binding.mountPath,
                  cacheId: binding.cache.id,
                  sharing: "locked",
                }),
              ),
              secretMounts: step.secrets.map((binding) =>
                create(SecretMountBindingSchema, {
                  dst: binding.mountPath,
                  secretRef: create(SecretRefSchema, { name: binding.secret }),
                }),
              ),
            }),
          },
        })
      case "copy":
        return this.compileCopyStep(step.dest, step.source)
      case "copyFrom":
        return create(ImageStepSchema, {
          kind: {
            case: "copyFromImage",
            value: create(CopyFromImageSchema, {
              dst: step.dest,
              srcImageKey: compileProvisionalImageKey(this.compileImage(step.source as ImageBuilderImpl)),
              srcPath: step.srcPath,
            }),
          },
        })
      case "workdir":
        return create(ImageStepSchema, {
          kind: { case: "workdir", value: create(WorkdirSchema, { path: step.path }) },
        })
      case "env":
        return create(ImageStepSchema, {
          kind: { case: "env", value: create(EnvSchema, { key: step.key, value: step.value }) },
        })
      case "user":
        return create(ImageStepSchema, {
          kind: { case: "user", value: create(UserSchema, { name: step.name }) },
        })
    }
  }

  compileCopyStep(dest: string, source: ImageCopyInput): ImageStep {
    if (isSourceFileRef(source)) {
      return create(ImageStepSchema, {
        kind: {
          case: "copySourceFile",
          value: create(CopySourceFileSchema, {
            dst: dest,
            srcRef: create(SourceFileRefSchema, { path: source.path }),
          }),
        },
      })
    }
    if (isSourceDirRef(source)) {
      return create(ImageStepSchema, {
        kind: {
          case: "copySourceDir",
          value: create(CopySourceDirSchema, {
            dst: dest,
            srcRef: this.compileSourceDirRef(source),
            ignore: [...source.ignore],
          }),
        },
      })
    }
    if (isImageBuilder(source)) {
      return create(ImageStepSchema, {
        kind: {
          case: "copyFromImage",
          value: create(CopyFromImageSchema, {
            dst: dest,
            srcImageKey: compileProvisionalImageKey(this.compileImage(source)),
            srcPath: "/",
          }),
        },
      })
    }
    throw new Error(
      "image.copy() source must be source.file(), source.directory(), or image()",
    )
  }

  compileSourceDirRef(ref: { readonly path: string; readonly ignore: readonly string[] }) {
    return create(SourceDirRefSchema, { path: ref.path, ignore: [...ref.ignore] })
  }
}

function currentArchitecture(): string {
  switch (process.arch) {
    case "arm64":
      return "arm64"
    case "x64":
      return "amd64"
    default:
      return process.arch
  }
}

function compileProvisionalImageKey(image: ImageSpec): string {
  // This is a compile-time reference for resolving Bundle.sub_images. BuildKit
  // computes source digests from the checked-out GitHub source when it builds.
  return canonicalImageKey(image)
}

function canonicalImageKey(image: ImageSpec): string {
  const hash = createHash("sha256")
  hash.update(IMAGE_KEY_DOMAIN)
  hash.update(u32be(image.formatVersion))
  hashLenPrefixedBytes(
    hash,
    image.platform ? toBinary(PlatformSchema, image.platform) : new Uint8Array(),
  )
  hashLenPrefixedBytes(hash, encodeImageSteps(image.steps))
  hashLenPrefixedBytes(hash, encodeDigestList(sourceInputDigests(image.steps)))
  hashLenPrefixedBytes(hash, encodeDigestList(subImageKeys(image.steps)))
  return `sha256:${hash.digest("hex")}`
}

function encodeImageSteps(steps: readonly ImageStep[]): Uint8Array {
  const chunks = [u64be(steps.length)]
  for (const step of steps) {
    chunks.push(lenPrefixedBytes(toBinary(ImageStepSchema, step)))
  }
  return concatBytes(chunks)
}

function encodeDigestList(values: readonly string[]): Uint8Array {
  const chunks = [u64be(values.length)]
  for (const value of values) {
    chunks.push(lenPrefixedBytes(new TextEncoder().encode(value)))
  }
  return concatBytes(chunks)
}

function sourceInputDigests(steps: readonly ImageStep[]): string[] {
  const values: string[] = []
  for (const step of steps) {
    switch (step.kind.case) {
      case "copySourceFile":
        values.push(step.kind.value.digest)
        break
      case "copySourceDir":
        values.push(step.kind.value.treeDigest)
        break
    }
  }
  return values
}

function subImageKeys(steps: readonly ImageStep[]): string[] {
  const values: string[] = []
  for (const step of steps) {
    if (step.kind.case === "copyFromImage") {
      values.push(step.kind.value.srcImageKey)
    }
  }
  return values
}

function hashLenPrefixedBytes(hash: ReturnType<typeof createHash>, bytes: Uint8Array): void {
  hash.update(u64be(bytes.byteLength))
  hash.update(bytes)
}

function lenPrefixedBytes(bytes: Uint8Array): Uint8Array {
  return concatBytes([u64be(bytes.byteLength), bytes])
}

function u32be(value: number): Uint8Array {
  const buffer = Buffer.alloc(4)
  buffer.writeUInt32BE(value)
  return buffer
}

function u64be(value: number): Uint8Array {
  const buffer = Buffer.alloc(8)
  buffer.writeBigUInt64BE(BigInt(value))
  return buffer
}

function concatBytes(chunks: readonly Uint8Array[]): Uint8Array {
  const total = chunks.reduce((sum, chunk) => sum + chunk.byteLength, 0)
  const out = new Uint8Array(total)
  let offset = 0
  for (const chunk of chunks) {
    out.set(chunk, offset)
    offset += chunk.byteLength
  }
  return out
}

function readSecretDecls(value: unknown): Record<string, Placement> {
  if (value === undefined) {
    return {}
  }
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("task secrets must be an object literal")
  }
  const record = value as Record<string, unknown>
  return Object.fromEntries(
    Object.entries(record).map(([name, placement]) => {
      validateSecretName(name, `task secrets.${name}`)
      return [name, readPlacement(placement, `task secrets.${name}`)]
    }),
  )
}

function readPlacement(value: unknown, label: string): Placement {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be a placement object`)
  }
  const record = value as Record<string, unknown>
  if ("env" in record) {
    const env = record["env"]
    if (Object.keys(record).length !== 1 || !isNonEmptyPlacementString(env)) {
      throw new Error(`${label} must be { env: string }`)
    }
    return { env }
  }
  if ("file" in record) {
    const file = record["file"]
    const mode = readOptionalPlacementString(record, "mode")
    const owner = readOptionalPlacementString(record, "owner")
    if (!hasOnlyKeys(record, ["file", "mode", "owner"]) || !isNonEmptyPlacementString(file)) {
      throw new Error(`${label} must be { file: string, mode?: string, owner?: string }`)
    }
    if (mode === INVALID_PLACEMENT_STRING || owner === INVALID_PLACEMENT_STRING) {
      throw new Error(`${label} must be { file: string, mode?: string, owner?: string }`)
    }
    validatePlacementPath(file, `${label}.file`)
    validatePlacementMode(mode, `${label}.mode`)
    return {
      file,
      ...(mode === undefined ? {} : { mode }),
      ...(owner === undefined ? {} : { owner }),
    }
  }
  if ("dir" in record) {
    const dir = record["dir"]
    const mode = readOptionalPlacementString(record, "mode")
    const owner = readOptionalPlacementString(record, "owner")
    if (!hasOnlyKeys(record, ["dir", "mode", "owner"]) || !isNonEmptyPlacementString(dir)) {
      throw new Error(`${label} must be { dir: string, mode?: string, owner?: string }`)
    }
    if (mode === INVALID_PLACEMENT_STRING || owner === INVALID_PLACEMENT_STRING) {
      throw new Error(`${label} must be { dir: string, mode?: string, owner?: string }`)
    }
    validatePlacementPath(dir, `${label}.dir`)
    validatePlacementMode(mode, `${label}.mode`)
    return {
      dir,
      ...(mode === undefined ? {} : { mode }),
      ...(owner === undefined ? {} : { owner }),
    }
  }
  throw new Error(
    `${label} must be one of { env }, { file, mode?, owner? }, or { dir, mode?, owner? }`,
  )
}

const INVALID_PLACEMENT_STRING = Symbol("invalid placement string")

function isNonEmptyPlacementString(value: unknown): value is string {
  return typeof value === "string" && value.trim() !== ""
}

function readOptionalPlacementString(
  record: Record<string, unknown>,
  key: "mode" | "owner",
): string | undefined | typeof INVALID_PLACEMENT_STRING {
  const value = record[key]
  if (value === undefined) {
    return undefined
  }
  return typeof value === "string" ? value : INVALID_PLACEMENT_STRING
}

function validatePlacementMode(mode: string | undefined, label: string): void {
  if (mode === undefined) {
    return
  }
  const normalized = mode.trim().replace(/^0[oO]/, "")
  if (!/^[0-7]+$/.test(normalized)) {
    throw new Error(`${label} must be an octal permission mode`)
  }
  if (Number.parseInt(normalized, 8) > 0o777) {
    throw new Error(`${label} must only contain permission bits`)
  }
}

function validatePlacementPath(path: string, label: string): void {
  const normalized = path.trim().replaceAll("\\", "/")
  if (path !== path.trim()) {
    throw new Error(`${label} must not contain leading or trailing whitespace`)
  }
  if (normalized === "." || normalized === "/") {
    throw new Error(`${label} must target a file or directory`)
  }
  if (normalized.split("/").includes("..")) {
    throw new Error(`${label} must not contain parent components`)
  }
}

function hasOnlyKeys(record: Record<string, unknown>, allowed: readonly string[]): boolean {
  return Object.keys(record).every((key) => allowed.includes(key))
}
