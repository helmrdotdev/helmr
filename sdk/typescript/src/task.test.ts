import { expect, test } from "bun:test"

import { image, sandbox, task } from "./index"

const invalidTaskIds = [
  "a/b",
  "a?b",
  "a#b",
  "a%b",
  "a b",
  "å",
  "-foo",
  ".foo",
  "",
  "a".repeat(129),
] as const

const sb = sandbox("task-id").image(image("task-id").from("debian:trixie-slim"))

test("task rejects invalid ids through the schema validator", () => {
  for (const id of invalidTaskIds) {
    expect(() => task({ id, sandbox: sb, run: async () => null })).toThrow("task id must match")
  }
})

test("task accepts task id boundaries", () => {
  expect(task({ id: "a", sandbox: sb, run: async () => null }).id).toBe("a")
  const max = "a".repeat(128)
  expect(task({ id: max, sandbox: sb, run: async () => null }).id).toBe(max)
  expect(task({ id: "approvals.grant", sandbox: sb, run: async () => null }).id).toBe("approvals.grant")
})
