import { describe, expect, test } from "bun:test"

import { secrets } from "./index"
import { inspectSecretAddress } from "./internal"

describe("Secret addresses", () => {
  test("preserves one locally validated name behind a private brand", () => {
    const reference = secrets.fromName("GHCR_TOKEN")
    expect(reference.name).toBe("GHCR_TOKEN")
    expect(inspectSecretAddress(reference)).toBe("GHCR_TOKEN")
    expect(inspectSecretAddress({ name: "GHCR_TOKEN" })).toBeUndefined()
    expect(Object.isFrozen(reference)).toBe(true)
  })

  test("rejects names outside the Control contract", () => {
    for (const name of ["", "-bad", "bad/name", "bad name", "a".repeat(129)]) {
      expect(() => secrets.fromName(name)).toThrow("Secret name is invalid")
    }
  })
})
