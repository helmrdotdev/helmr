import { describe, expect, test } from "bun:test"

import { resourceID } from "./id"

describe("resourceID", () => {
  test("accepts canonical UUIDv7", () => {
    const id = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"
    expect(resourceID(id, "Run ID")).toBe(id)
  })

  test.each([
    "019C10D5-A6F7-7AF1-8F5F-BB97BCC0DC32",
    "019c10d5-a6f7-4af1-8f5f-bb97bcc0dc32",
    "019c10d5-a6f7-7af1-7f5f-bb97bcc0dc32",
    " 019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
  ])("rejects %s", (id) => {
    expect(() => resourceID(id, "Run ID")).toThrow(
      "Run ID must be a canonical UUIDv7",
    )
  })
})
