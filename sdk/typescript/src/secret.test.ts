import { describe, expect, test } from "bun:test"

import { image, secrets } from "./index"
import { inspectImage, inspectSecretAddress } from "./internal"

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

  test("binds image authentication only through a Secret address", () => {
    const declared = image("root").from("ghcr.io/acme/base:1", {
      auth: {
        username: "aktky",
        password: secrets.fromName("GHCR_TOKEN"),
      },
    })
    expect(inspectImage(declared)?.steps).toEqual([{
      kind: "from",
      ref: "ghcr.io/acme/base:1",
      auth: {
        username: "aktky",
        passwordSecret: "GHCR_TOKEN",
      },
    }])
    expect(() => image("root").from("ghcr.io/acme/base:1", {
      auth: {
        username: "aktky",
        password: { name: "GHCR_TOKEN" } as never,
      },
    })).toThrow("requires secrets.fromName()")
    expect(() => image("root").from("ghcr.io/acme/base:1", {
      auth: {
        username: "aktky",
        password: secrets.fromName("GHCR_TOKEN"),
        extra: true,
      } as never,
    })).toThrow("unknown members")
  })
})
