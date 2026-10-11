import { describe, expect, test } from "bun:test"

import { defineConfig } from "./config"
import { inspectConfig, matchesIgnorePattern } from "./internal"

describe("defineConfig", () => {
  test("normalizes and copies the closed config", () => {
    const dirs = ["./agents", "actors"]
    const ignorePatterns = ["agents/generated/**", "**/*.test.js"]
    const config = defineConfig({
      dirs,
      ignorePatterns,
    })
    dirs.push("./actors")
    ignorePatterns.push("actors/generated/**")
    expect(config).toMatchObject({
      dirs: ["actors", "agents"],
      ignorePatterns: ["**/*.test.js", "agents/generated/**"],
    })
    expect(Object.isFrozen(config)).toBe(true)
    expect(Object.isFrozen(config.dirs)).toBe(true)
    expect(Object.isFrozen(config.ignorePatterns)).toBe(true)
    expect(inspectConfig({ ...config })).toEqual(config)
  })

  test("normalizes explicit runtime packages and asset patterns without retaining mutable inputs", () => {
    const external = ["sharp", "@scope/tool"]
    const assets = ["./prompts/**", "templates/*.txt"]
    const config = defineConfig({ build: { external, assets } })
    external.push("other")
    assets.push("other/**")
    expect(config.build.external).toEqual(["@scope/tool", "sharp"])
    expect(config.build.assets).toEqual(["prompts/**", "templates/*.txt"])
    expect(Object.isFrozen(config.build.external)).toBe(true)
    expect(Object.isFrozen(config.build.assets)).toBe(true)
    for (const value of ["tool/subpath", "node:fs", "../tool", "@scope"]) {
      expect(() => defineConfig({ build: { external: [value] } })).toThrow()
    }
    for (const value of ["../prompts/**", "/prompts/**", "!prompts/**"]) {
      expect(() => defineConfig({ build: { assets: [value] } })).toThrow()
    }
  })

  test("requires the explicit config shape", () => {
    expect(() => defineConfig({ dirs: [] })).toThrow()
    expect(defineConfig({ dirs: ["agents"] })).toMatchObject({
      dirs: ["agents"],
      ignorePatterns: [],
    })
    expect(() =>
      defineConfig({ dirs: ["agents", "./agents"] }),
    ).toThrow("duplicate")
    expect(() =>
      defineConfig({
        dirs: ["./agents/../other"],
      }),
    ).toThrow()
    expect(() =>
      defineConfig({
        dirs: ["./agents//other"],
      }),
    ).toThrow()
    expect(() =>
      defineConfig({
        dirs: ["./agents/./other"],
      }),
    ).toThrow()
    expect(() =>
      defineConfig({
        dirs: ["./agents"],
        extra: true,
      } as never),
    ).toThrow()
    expect(() =>
      inspectConfig({
        dirs: ["agents"],
        ignorePatterns: undefined,
      }),
    ).toThrow()
    expect(() => defineConfig({ dirs: ["agents/\ud800"] })).toThrow()
  })

  test("rejects executable or exotic config values", () => {
    const accessor = {}
    Object.defineProperty(accessor, "dirs", {
      enumerable: true,
      get: () => ["agents"],
    })
    expect(() => inspectConfig(accessor)).toThrow("data properties")

    const customPrototype = Object.create({ inherited: true }) as {
      dirs: string[]
    }
    customPrototype.dirs = ["agents"]
    expect(() => inspectConfig(customPrototype)).toThrow("ordinary object")

    const symbol = Symbol("extra")
    expect(() =>
      inspectConfig({
        dirs: ["agents"],
        [symbol]: true,
      }),
    ).toThrow()

    const sparse = new Array<string>(1)
    expect(() => inspectConfig({ dirs: sparse })).toThrow("dense ordinary array")
  })

  test("does not trust intrinsics mutated by config code", () => {
    const arrayMap = Object.getOwnPropertyDescriptor(Array.prototype, "map")
    const arraySort = Object.getOwnPropertyDescriptor(Array.prototype, "sort")
    const arrayIndex = Object.getOwnPropertyDescriptor(Array.prototype, "0")
    const objectGetPrototypeOf = Object.getOwnPropertyDescriptor(
      Object,
      "getPrototypeOf",
    )
    const stringStartsWith = Object.getOwnPropertyDescriptor(
      String.prototype,
      "startsWith",
    )
    const stringSplit = Object.getOwnPropertyDescriptor(
      String.prototype,
      "split",
    )
    const textEncode = Object.getOwnPropertyDescriptor(
      TextEncoder.prototype,
      "encode",
    )
    let config: ReturnType<typeof inspectConfig> | undefined
    try {
      Object.defineProperty(Array.prototype, "map", {
        configurable: true,
        writable: true,
        value: () => ["../escape"],
      })
      Object.defineProperty(Array.prototype, "sort", {
        configurable: true,
        writable: true,
        value: () => undefined,
      })
      Object.defineProperty(Array.prototype, "0", {
        configurable: true,
        set: () => {
          throw new Error("mutated array index")
        },
      })
      Object.defineProperty(Object, "getPrototypeOf", {
        configurable: true,
        writable: true,
        value: () => null,
      })
      Object.defineProperty(String.prototype, "startsWith", {
        configurable: true,
        writable: true,
        value: () => {
          throw new Error("mutated startsWith")
        },
      })
      Object.defineProperty(String.prototype, "split", {
        configurable: true,
        writable: true,
        value: () => ["../escape"],
      })
      Object.defineProperty(TextEncoder.prototype, "encode", {
        configurable: true,
        writable: true,
        value: () => new Uint8Array(),
      })
      config = inspectConfig({
        dirs: ["agents/z", "./agents/a"],
        ignorePatterns: ["agents/z/**", "**/*.test.js"],
      })
    } finally {
      Object.defineProperty(Array.prototype, "map", arrayMap as PropertyDescriptor)
      Object.defineProperty(Array.prototype, "sort", arraySort as PropertyDescriptor)
      if (arrayIndex === undefined) {
        delete Array.prototype[0]
      } else {
        Object.defineProperty(Array.prototype, "0", arrayIndex)
      }
      Object.defineProperty(
        Object,
        "getPrototypeOf",
        objectGetPrototypeOf as PropertyDescriptor,
      )
      Object.defineProperty(
        String.prototype,
        "startsWith",
        stringStartsWith as PropertyDescriptor,
      )
      Object.defineProperty(
        String.prototype,
        "split",
        stringSplit as PropertyDescriptor,
      )
      Object.defineProperty(
        TextEncoder.prototype,
        "encode",
        textEncode as PropertyDescriptor,
      )
    }
    expect(config).toMatchObject({
      dirs: ["agents/a", "agents/z"],
      ignorePatterns: ["**/*.test.js", "agents/z/**"],
    })
  })

  test("accepts only the closed glob subset", () => {
    for (const pattern of [
      "",
      "./agents/**",
      "/agents/**",
      "agents/",
      "agents//file.ts",
      "agents/../file.ts",
      "agents/[ab].ts",
      "agents/{a,b}.ts",
      "agents/**file.ts",
      "!agents/file.ts",
      "agents/@(a).ts",
    ]) {
      expect(() =>
        defineConfig({
          dirs: ["./agents"],
          ignorePatterns: [pattern],
        }),
      ).toThrow()
    }
  })
})

describe("ignore pattern matching", () => {
  test("implements segment-local stars and zero-or-more segment globstars", () => {
    expect(matchesIgnorePattern("agents/*.ts", "agents/a.ts")).toBe(true)
    expect(matchesIgnorePattern("agents/*.ts", "agents/nested/a.ts")).toBe(false)
    expect(matchesIgnorePattern("agents/**/generated?.ts", "agents/generated1.ts")).toBe(true)
    expect(matchesIgnorePattern("agents/**/generated?.ts", "agents/a/b/generated1.ts")).toBe(true)
    expect(matchesIgnorePattern("agents/**", "agents/a.ts")).toBe(true)
    expect(matchesIgnorePattern("agents/**", "agents")).toBe(false)
    expect(matchesIgnorePattern("**/*.test.ts", ".hidden/a.test.ts")).toBe(true)
    expect(matchesIgnorePattern("agents/?/a.ts", "agents/🦀/a.ts")).toBe(true)
  })
})

test("empty config uses defaults and unknown options are rejected", () => {
 expect(defineConfig({})).toMatchObject({dirs:["agents"],ignorePatterns:[]})
 expect(() => inspectConfig({unexpectedOption:true})).toThrow()
})
