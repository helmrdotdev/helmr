import { describe, expect, test } from "bun:test"

import { defineConfig } from "./config"
import { inspectConfig, matchesIgnorePattern } from "./internal"

describe("defineConfig", () => {
  test("normalizes and copies the closed config", () => {
    const dirs = ["./tasks", "actors"]
    const ignorePatterns = ["tasks/generated/**", "**/*.test.js"]
    const config = defineConfig({
      dirs,
      ignorePatterns,
    })
    dirs.push("./actors")
    ignorePatterns.push("actors/generated/**")
    expect(config).toEqual({
      dirs: ["actors", "tasks"],
      ignorePatterns: ["**/*.test.js", "tasks/generated/**"],
    })
    expect(Object.isFrozen(config)).toBe(true)
    expect(Object.isFrozen(config.dirs)).toBe(true)
    expect(Object.isFrozen(config.ignorePatterns)).toBe(true)
    expect(inspectConfig({ ...config })).toEqual(config)
  })

  test("requires the explicit config shape", () => {
    expect(() => defineConfig({ dirs: [] })).toThrow()
    expect(defineConfig({ dirs: ["tasks"] })).toEqual({
      dirs: ["tasks"],
      ignorePatterns: [],
    })
    expect(() =>
      defineConfig({ dirs: ["tasks", "./tasks"] }),
    ).toThrow("duplicate")
    expect(() =>
      defineConfig({
        dirs: ["./tasks/../other"],
      }),
    ).toThrow()
    expect(() =>
      defineConfig({
        dirs: ["./tasks//other"],
      }),
    ).toThrow()
    expect(() =>
      defineConfig({
        dirs: ["./tasks/./other"],
      }),
    ).toThrow()
    expect(() =>
      defineConfig({
        dirs: ["./tasks"],
        extra: true,
      } as never),
    ).toThrow()
    expect(() =>
      inspectConfig({
        dirs: ["tasks"],
        ignorePatterns: undefined,
      }),
    ).toThrow()
    expect(() => defineConfig({ dirs: ["tasks/\ud800"] })).toThrow()
  })

  test("rejects executable or exotic config values", () => {
    const accessor = {}
    Object.defineProperty(accessor, "dirs", {
      enumerable: true,
      get: () => ["tasks"],
    })
    expect(() => inspectConfig(accessor)).toThrow("data properties")

    const customPrototype = Object.create({ inherited: true }) as {
      dirs: string[]
    }
    customPrototype.dirs = ["tasks"]
    expect(() => inspectConfig(customPrototype)).toThrow("ordinary object")

    const symbol = Symbol("extra")
    expect(() =>
      inspectConfig({
        dirs: ["tasks"],
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
        dirs: ["tasks/z", "./tasks/a"],
        ignorePatterns: ["tasks/z/**", "**/*.test.js"],
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
    expect(config).toEqual({
      dirs: ["tasks/a", "tasks/z"],
      ignorePatterns: ["**/*.test.js", "tasks/z/**"],
    })
  })

  test("accepts only the closed glob subset", () => {
    for (const pattern of [
      "",
      "./tasks/**",
      "/tasks/**",
      "tasks/",
      "tasks//file.ts",
      "tasks/../file.ts",
      "tasks/[ab].ts",
      "tasks/{a,b}.ts",
      "tasks/**file.ts",
      "!tasks/file.ts",
      "tasks/@(a).ts",
    ]) {
      expect(() =>
        defineConfig({
          dirs: ["./tasks"],
          ignorePatterns: [pattern],
        }),
      ).toThrow()
    }
  })
})

describe("ignore pattern matching", () => {
  test("implements segment-local stars and zero-or-more segment globstars", () => {
    expect(matchesIgnorePattern("tasks/*.ts", "tasks/a.ts")).toBe(true)
    expect(matchesIgnorePattern("tasks/*.ts", "tasks/nested/a.ts")).toBe(false)
    expect(matchesIgnorePattern("tasks/**/generated?.ts", "tasks/generated1.ts")).toBe(true)
    expect(matchesIgnorePattern("tasks/**/generated?.ts", "tasks/a/b/generated1.ts")).toBe(true)
    expect(matchesIgnorePattern("tasks/**", "tasks/a.ts")).toBe(true)
    expect(matchesIgnorePattern("tasks/**", "tasks")).toBe(false)
    expect(matchesIgnorePattern("**/*.test.ts", ".hidden/a.test.ts")).toBe(true)
    expect(matchesIgnorePattern("tasks/?/a.ts", "tasks/🦀/a.ts")).toBe(true)
  })
})
