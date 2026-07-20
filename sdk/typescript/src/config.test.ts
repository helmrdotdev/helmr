import { describe, expect, test } from "bun:test"

import { defineConfig } from "./config"
import { inspectConfig, matchesIgnorePattern } from "./internal"

describe("defineConfig", () => {
  test("brands and copies the closed config", () => {
    const dirs = ["./tasks"]
    const ignorePatterns = ["tasks/generated/**"]
    const config = defineConfig({
      project: "helmr",
      dirs,
      ignorePatterns,
    })
    dirs.push("./actors")
    ignorePatterns.push("actors/generated/**")
    expect(config).toEqual({
      project: "helmr",
      dirs: ["./tasks"],
      ignorePatterns: ["tasks/generated/**"],
    })
    expect(inspectConfig(config)).toBe(config)
    expect(inspectConfig({ ...config })).toBeUndefined()
  })

  test("requires the explicit config shape", () => {
    expect(() => defineConfig({ project: "helmr", dirs: [] })).toThrow()
    expect(() => defineConfig({ project: "helmr", dirs: ["tasks"] })).toThrow()
    expect(() =>
      defineConfig({
        project: "helmr",
        dirs: ["./tasks/../other"],
      }),
    ).toThrow()
    expect(() =>
      defineConfig({
        project: "helmr",
        dirs: ["./tasks//other"],
      }),
    ).toThrow()
    expect(() =>
      defineConfig({
        project: "helmr",
        dirs: ["./tasks/./other"],
      }),
    ).toThrow()
    expect(() =>
      defineConfig({
        project: "helmr",
        dirs: ["./tasks"],
        extra: true,
      } as never),
    ).toThrow()
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
          project: "helmr",
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
