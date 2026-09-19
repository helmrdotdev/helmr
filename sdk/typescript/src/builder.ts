const builderBrand = Symbol.for("helmr.sdk.v0.builder")

declare const builderTypeBrand: unique symbol

// Builder prepares the Helmr-managed Linux environment in which dependencies
// are installed and declarations are analyzed. It is a separate role from
// image(): it has no base, key or Workspace identity, and its copy sources are
// paths in the captured project source rather than the installed tree.
export interface Builder {
  readonly [builderTypeBrand]: true
  readonly steps: readonly BuilderStep[]
  run(argv: readonly string[]): Builder
  copy(source: string, destination: string): Builder
}

export type BuilderStep =
  | Readonly<{ kind: "run"; argv: readonly string[] }>
  | Readonly<{ kind: "copy"; source: string; destination: string }>

class BuilderValue {
  readonly steps: readonly BuilderStep[]

  constructor(steps: readonly BuilderStep[] = []) {
    this.steps = Object.freeze([...steps])
    Object.defineProperty(this, builderBrand, { value: true })
    Object.freeze(this)
  }

  run(argv: readonly string[], ...unexpected: readonly unknown[]): Builder {
    if (unexpected.length !== 0) {
      throw new Error("builder.run() accepts only argv")
    }
    if (!Array.isArray(argv) || argv.some((argument) => typeof argument !== "string")) {
      throw new Error("builder.run() requires an argv array of strings")
    }
    return new BuilderValue([
      ...this.steps,
      Object.freeze({ kind: "run", argv: Object.freeze([...argv]) }),
    ]) as unknown as Builder
  }

  copy(
    source: string,
    destination: string,
    ...unexpected: readonly unknown[]
  ): Builder {
    if (unexpected.length !== 0) {
      throw new Error("builder.copy() accepts only a source and a destination")
    }
    if (typeof source !== "string") {
      throw new Error(
        "builder.copy() requires a project source path as its first argument; source.file() and source.directory() belong to image.copy()",
      )
    }
    if (typeof destination !== "string") {
      throw new Error("builder.copy() requires a destination path as its second argument")
    }
    if (/[*?\\]/.test(source)) {
      throw new Error(
        "builder.copy() sources are literal project paths; '*', '?' and '\\' are not supported, copy the containing directory instead",
      )
    }
    return new BuilderValue([
      ...this.steps,
      Object.freeze({ kind: "copy", source, destination }),
    ]) as unknown as Builder
  }
}

// The public signature takes nothing; plain JavaScript callers that pass a
// base image or options still get an error instead of being ignored.
export function builder(): Builder
export function builder(...unexpected: readonly unknown[]): Builder {
  if (unexpected.length !== 0) {
    throw new Error("builder() takes no arguments; it always starts from the Helmr builder image")
  }
  return new BuilderValue() as unknown as Builder
}

export function isBuilder(value: unknown): value is Builder {
  return (
    typeof value === "object" &&
    value !== null &&
    (value as Record<PropertyKey, unknown>)[builderBrand] === true
  )
}
