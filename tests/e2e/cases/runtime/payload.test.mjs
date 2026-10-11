import { expect, test } from "bun:test"
import { runtimeSmokePayload } from "./task.ts"

test("runtime input validates application options and rejects unknown fields", () => {
  expect(runtimeSmokePayload.parse({ exerciseQuestion: true }).exerciseQuestion).toBe(true)
  expect(runtimeSmokePayload.safeParse({ unknownOption: true }).success).toBe(false)
  expect(runtimeSmokePayload.safeParse({ largeFileKiB: 4097 }).success).toBe(false)
})
