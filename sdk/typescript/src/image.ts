import {
  inspectSecretAddress,
  type SecretAddress,
} from "./secret"

const imageBrand = Symbol.for("helmr.sdk.v0.image")
const sourceFileBrand = Symbol.for("helmr.sdk.v0.source-file")
const sourceDirectoryBrand = Symbol.for("helmr.sdk.v0.source-directory")

export interface SourceFileRef {
  readonly path: string
}

export interface SourceDirectoryRef {
  readonly path: string
}

export type ImageCopyInput = SourceFileRef | SourceDirectoryRef | ImageBuilder

export interface ImageRegistryAuth {
  readonly username: string
  readonly password: SecretAddress
}

export interface ImageFromOptions {
  readonly auth?: ImageRegistryAuth
}

export interface ImageBuilder {
  readonly key: string
  from(ref: string, options?: ImageFromOptions): ImageBuilder
  run(argv: readonly string[]): ImageBuilder
  copy(destination: string, source: SourceFileRef | SourceDirectoryRef): ImageBuilder
  copyFrom(
    destination: string,
    source: ImageBuilder,
    sourcePath: string,
  ): ImageBuilder
  workdir(path: string): ImageBuilder
  env(key: string, value: string): ImageBuilder
  user(name: string): ImageBuilder
}

export type InternalImageStep =
  | Readonly<{
      kind: "from"
      ref: string
      auth?: Readonly<{
        username: string
        passwordSecret: string
      }>
    }>
  | Readonly<{
      kind: "run"
      argv: readonly string[]
    }>
  | Readonly<{
      kind: "copy_source_file"
      destination: string
      source: SourceFileRef
    }>
  | Readonly<{
      kind: "copy_source_directory"
      destination: string
      source: SourceDirectoryRef
    }>
  | Readonly<{
      kind: "copy_from_image"
      destination: string
      source: ImageBuilder
      sourcePath: string
    }>
  | Readonly<{ kind: "workdir"; path: string }>
  | Readonly<{ kind: "env"; key: string; value: string }>
  | Readonly<{ kind: "user"; name: string }>

export interface InternalImage {
  readonly key: string
  readonly steps: readonly InternalImageStep[]
}

class Image implements ImageBuilder {
  readonly key: string
  readonly steps: readonly InternalImageStep[]

  constructor(key: string, steps: readonly InternalImageStep[] = []) {
    if (key.length === 0) {
      throw new Error("image key must be non-empty")
    }
    this.key = key
    this.steps = Object.freeze([...steps])
    Object.defineProperty(this, imageBrand, { value: true })
    Object.freeze(this)
  }

  from(ref: string, options?: ImageFromOptions): ImageBuilder {
    if (options === undefined) {
      return new Image(this.key, [...this.steps, { kind: "from", ref }])
    }
    assertExactMembers(options, ["auth"], "image.from() options")
    if (options.auth === undefined) {
      throw new Error("image.from() options.auth is required")
    }
    assertExactMembers(
      options.auth,
      ["password", "username"],
      "image.from() auth",
    )
    validateRegistryUsername(options.auth.username)
    const passwordSecret = inspectSecretAddress(options.auth.password)
    if (passwordSecret === undefined) {
      throw new Error("image.from() auth.password requires secrets.fromName()")
    }
    return new Image(this.key, [
      ...this.steps,
      {
        kind: "from",
        ref,
        auth: Object.freeze({
          username: options.auth.username,
          passwordSecret,
        }),
      },
    ])
  }

  run(
    argv: readonly string[],
    ...unexpected: readonly unknown[]
  ): ImageBuilder {
    if (unexpected.length !== 0) {
      throw new Error("image.run() accepts only argv")
    }
    return new Image(this.key, [
      ...this.steps,
      {
        kind: "run",
        argv: Object.freeze([...argv]),
      },
    ])
  }

  copy(
    destination: string,
    source: SourceFileRef | SourceDirectoryRef,
  ): ImageBuilder {
    if (isSourceFileRef(source)) {
      return new Image(this.key, [
        ...this.steps,
        { kind: "copy_source_file", destination, source },
      ])
    }
    if (isSourceDirectoryRef(source)) {
      return new Image(this.key, [
        ...this.steps,
        { kind: "copy_source_directory", destination, source },
      ])
    }
    throw new Error("image.copy() requires source.file() or source.directory()")
  }

  copyFrom(
    destination: string,
    source: ImageBuilder,
    sourcePath: string,
  ): ImageBuilder {
    if (inspectImage(source) === undefined) {
      throw new Error("image.copyFrom() requires an image() value")
    }
    return new Image(this.key, [
      ...this.steps,
      { kind: "copy_from_image", destination, source, sourcePath },
    ])
  }

  workdir(path: string): ImageBuilder {
    return new Image(this.key, [...this.steps, { kind: "workdir", path }])
  }

  env(key: string, value: string): ImageBuilder {
    return new Image(this.key, [...this.steps, { kind: "env", key, value }])
  }

  user(name: string): ImageBuilder {
    return new Image(this.key, [...this.steps, { kind: "user", name }])
  }
}

class SourceFile implements SourceFileRef {
  readonly path: string
  constructor(path: string) {
    this.path = path
    Object.defineProperty(this, sourceFileBrand, { value: true })
    Object.freeze(this)
  }
}

class SourceDirectory implements SourceDirectoryRef {
  readonly path: string
  constructor(path: string) {
    this.path = path
    Object.defineProperty(this, sourceDirectoryBrand, { value: true })
    Object.freeze(this)
  }
}

export function image(key: string): ImageBuilder {
  return new Image(key)
}

export const source = Object.freeze({
  file(path: string): SourceFileRef {
    return new SourceFile(path)
  },
  directory(path: string): SourceDirectoryRef {
    return new SourceDirectory(path)
  },
})

export function inspectImage(value: unknown): InternalImage | undefined {
  if (
    typeof value !== "object" ||
    value === null ||
    (value as Record<PropertyKey, unknown>)[imageBrand] !== true
  ) {
    return undefined
  }
  const imageValue = value as Image
  return { key: imageValue.key, steps: imageValue.steps }
}

function isSourceFileRef(value: unknown): value is SourceFileRef {
  return (
    typeof value === "object" &&
    value !== null &&
    (value as Record<PropertyKey, unknown>)[sourceFileBrand] === true
  )
}

function isSourceDirectoryRef(value: unknown): value is SourceDirectoryRef {
  return (
    typeof value === "object" &&
    value !== null &&
    (value as Record<PropertyKey, unknown>)[sourceDirectoryBrand] === true
  )
}

function assertExactMembers(
  value: unknown,
  expected: readonly string[],
  label: string,
): void {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  const actual = Object.keys(value).sort()
  if (
    actual.length !== expected.length ||
    actual.some((key, index) => key !== expected[index])
  ) {
    throw new Error(`${label} has unknown members`)
  }
}

function validateRegistryUsername(value: unknown): asserts value is string {
  if (
    typeof value !== "string" ||
    value.length === 0 ||
    value.trim() !== value ||
    new TextEncoder().encode(value).length > 256 ||
    /[\0-\x1f\x7f-\x9f]/u.test(value)
  ) {
    throw new Error("image.from() auth.username is invalid")
  }
}
