import { expect, test } from "bun:test"

import { defineConfig } from "./config"

test("defineConfig requires at least one task directory", () => {
  expect(() => defineConfig({} as never)).toThrow("requires a non-empty dirs array")
  expect(() => defineConfig({ dirs: [] })).toThrow("requires a non-empty dirs array")
})

test("defineConfig copies dirs and ignore patterns", () => {
  const dirs = ["./tasks"]
  const ignorePatterns = ["**/*.fixture.ts"]

  const config = defineConfig({ project: "local-deploys", dirs, ignorePatterns })
  dirs.push("./other")
  ignorePatterns.push("**/*.test.ts")

  expect(config.project).toBe("local-deploys")
  expect(config.dirs).toEqual(["./tasks"])
  expect(config.ignorePatterns).toEqual(["**/*.fixture.ts"])
})

test("defineConfig rejects invalid entries", () => {
  expect(() => defineConfig({ project: "", dirs: ["tasks"] })).toThrow(
    "defineConfig({ project }) must be a non-empty string",
  )
  expect(() => defineConfig({ project: "   ", dirs: ["tasks"] })).toThrow(
    "defineConfig({ project }) must be a non-empty string",
  )
  expect(() => defineConfig({ project: "local\0deploys", dirs: ["tasks"] })).toThrow(
    "defineConfig({ project }) must not contain NUL",
  )
  expect(() => defineConfig({ dirs: [""] })).toThrow("entries must be non-empty strings")
  expect(() => defineConfig({ dirs: ["tasks\0"] })).toThrow("entries must not contain NUL")
  expect(() => defineConfig({ dirs: ["tasks"], ignorePatterns: [""] })).toThrow(
    "entries must be non-empty strings",
  )
  expect(() => defineConfig({ dirs: ["tasks"], ignorePatterns: ["**\0"] })).toThrow(
    "entries must not contain NUL",
  )
})
