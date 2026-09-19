import { describe, expect, test } from "bun:test"
import { builder, defineConfig, image, sandbox, source } from "./index"
import { inspectConfig, isBuilder } from "./internal"

describe("builder", () => {
  test("records ordered immutable steps and lets branches share a prefix", () => {
    const base = builder().copy("build/setup.sh", "/opt/setup.sh")
    const left = base.run(["/bin/sh", "/opt/setup.sh", "left"])
    const right = base.run(["/bin/sh", "/opt/setup.sh", "right"])
    expect(base.steps).toEqual([{ kind: "copy", source: "build/setup.sh", destination: "/opt/setup.sh" }])
    expect(left.steps[1]).toEqual({ kind: "run", argv: ["/bin/sh", "/opt/setup.sh", "left"] })
    expect(right.steps[1]).toEqual({ kind: "run", argv: ["/bin/sh", "/opt/setup.sh", "right"] })
    expect(Object.isFrozen(base) && Object.isFrozen(base.steps) && Object.isFrozen(left.steps[1])).toBe(true)
    const argv = ["echo", "one"]
    const recorded = builder().run(argv)
    argv[1] = "two"
    expect(recorded.steps).toEqual([{ kind: "run", argv: ["echo", "one"] }])
  })

  test("offers only run and copy", () => {
    const value = builder() as unknown as Record<string, unknown>
    for (const method of ["from", "copyFrom", "workdir", "env", "user"]) {
      expect(value[method]).toBeUndefined()
    }
  })

  test("rejects calls that are not run(argv) or copy(source path, destination)", () => {
    const call = (method: "run" | "copy", ...args: unknown[]) => () =>
      (builder()[method] as (...args: unknown[]) => unknown).apply(builder(), args)
    expect(call("run", "apt-get update")).toThrow("argv array of strings")
    expect(call("run", ["ok", 1])).toThrow("argv array of strings")
    expect(call("run", ["ok"], { shell: true })).toThrow("only argv")
    expect(call("copy", source.file("build/setup.sh"), "/opt/setup.sh")).toThrow("belong to image.copy()")
    expect(call("copy", "build/setup.sh")).toThrow("destination path as its second argument")
    expect(call("copy", "a", "/b", "/c")).toThrow("only a source and a destination")
    for (const pattern of ["build/*.sh", "build/setup.s?", "build\\setup.sh"]) {
      expect(call("copy", pattern, "/opt/x")).toThrow("literal project paths")
    }
    expect(builder().copy("assets[1]/a[1].txt", "/opt/a.txt").steps).toHaveLength(1)
    expect(() => (builder as (...args: unknown[]) => unknown)("debian:bookworm")).toThrow("takes no arguments")
  })

  test("is a different role from a Workspace image", () => {
    expect(isBuilder(builder())).toBe(true)
    expect(isBuilder(image("workspace"))).toBe(false)
    expect(() => sandbox({ id: "wrong-role" }).image(builder() as never)).toThrow()
    expect(() => defineConfig({ build: { builder: image("workspace").from("debian") as never } }))
      .toThrow("must be created by builder()")
  })
})

describe("config build settings", () => {
  test("defaults to the unmodified builder, inferred install and no secrets", () => {
    const config = defineConfig({ dirs: ["src"] })
    expect(config.build.builder.steps).toEqual([])
    expect(config.build.installCommand).toBeUndefined()
    expect(config.build.secrets).toEqual([])
  })

  test("normalizes settings and keeps the builder a branded value", () => {
    const prepared = builder().run(["apt-get", "install", "-y", "jq"])
    const config = defineConfig({
      dirs: ["src"],
      build: { builder: prepared, installCommand: "./prepare.sh --frozen", secrets: ["NPM_TOKEN", "GITHUB_TOKEN"] },
    })
    expect(config.build.builder).toBe(prepared)
    expect(config.build.installCommand).toBe("./prepare.sh --frozen")
    expect(config.build.secrets).toEqual(["GITHUB_TOKEN", "NPM_TOKEN"])
    // A normalized config inspects to the same result, as when another SDK copy reads it.
    const again = inspectConfig(config)
    expect(again.build.builder).toBe(prepared)
    expect(again).toEqual(config)
  })

  test("rejects plain step data, unknown keys, secret values and empty commands", () => {
    const steps = [{ kind: "run", argv: ["true"] }]
    expect(() => inspectConfig({ build: { builder: { steps } } })).toThrow("must be created by builder()")
    expect(() => inspectConfig({ build: { packages: ["jq"] } })).toThrow("accepts only builder, installCommand and secrets")
    expect(() => inspectConfig({ builder: builder() })).toThrow("accepts only dirs, ignorePatterns and build")
    expect(() => defineConfig({ build: { secrets: ["npm-token"] } })).toThrow("environment variable names")
    expect(() => defineConfig({ build: { secrets: ["TOKEN", "TOKEN"] } })).toThrow("duplicate")
    expect(() => defineConfig({ build: { installCommand: "  " } })).toThrow("non-empty command")
  })
})
