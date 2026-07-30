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

export interface ImageBuilder {
  readonly id: string
  from(ref: string): ImageBuilder
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
  | Readonly<{ kind: "from"; ref: string }>
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
  readonly id: string
  readonly steps: readonly InternalImageStep[]
}

class Image implements ImageBuilder {
  readonly id: string
  readonly steps: readonly InternalImageStep[]

  constructor(id: string, steps: readonly InternalImageStep[] = []) {
    if (id.length === 0) {
      throw new Error("image id must be non-empty")
    }
    this.id = id
    this.steps = Object.freeze([...steps])
    Object.defineProperty(this, imageBrand, { value: true })
    Object.freeze(this)
  }

  from(ref: string): ImageBuilder {
    return new Image(this.id, [...this.steps, { kind: "from", ref }])
  }

  run(
    argv: readonly string[],
    ...unexpected: readonly unknown[]
  ): ImageBuilder {
    if (unexpected.length !== 0) {
      throw new Error("image.run() accepts only argv")
    }
    return new Image(this.id, [
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
      return new Image(this.id, [
        ...this.steps,
        { kind: "copy_source_file", destination, source },
      ])
    }
    if (isSourceDirectoryRef(source)) {
      return new Image(this.id, [
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
    return new Image(this.id, [
      ...this.steps,
      { kind: "copy_from_image", destination, source, sourcePath },
    ])
  }

  workdir(path: string): ImageBuilder {
    return new Image(this.id, [...this.steps, { kind: "workdir", path }])
  }

  env(key: string, value: string): ImageBuilder {
    return new Image(this.id, [...this.steps, { kind: "env", key, value }])
  }

  user(name: string): ImageBuilder {
    return new Image(this.id, [...this.steps, { kind: "user", name }])
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

export function image(id: string): ImageBuilder {
  return new Image(id)
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
  return { id: imageValue.id, steps: imageValue.steps }
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
