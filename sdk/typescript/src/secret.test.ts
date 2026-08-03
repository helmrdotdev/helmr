import { describe, expect, test } from "bun:test"

import { image, secrets } from "./index"
import { inspectImage, inspectSecretNameRef } from "./internal"
import { createClientWorkspaces } from "./workspace"

describe("Secret name references", () => {
  test("preserves one locally validated name behind a private brand", () => {
    const reference = secrets.fromName("GHCR_TOKEN")
    expect(reference.name).toBe("GHCR_TOKEN")
    expect(inspectSecretNameRef(reference)).toBe("GHCR_TOKEN")
    expect(inspectSecretNameRef({ name: "GHCR_TOKEN" })).toBeUndefined()
    expect(Object.isFrozen(reference)).toBe(true)
  })

  test("rejects names outside the Control contract", () => {
    for (const name of ["", "-bad", "bad/name", "bad name", "a".repeat(129)]) {
      expect(() => secrets.fromName(name)).toThrow("Secret name is invalid")
    }
  })

  test("binds image authentication only through a Secret name reference", () => {
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

  test("rejects a forged Workspace Secret before transport", async () => {
    let called = false
    const workspaces = createClientWorkspaces({
      async request() {
        called = true
        return { workspace_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32" }
      },
    })
    await expect(workspaces.create("machine", {
      secrets: [{
        secret: { name: "TOKEN" } as never,
        env: "TOKEN",
      }],
    })).rejects.toThrow("requires secrets.fromName()")
    expect(called).toBe(false)
  })
})
