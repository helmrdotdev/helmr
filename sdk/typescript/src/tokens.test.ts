import { describe, expect, test } from "bun:test"

import { installRuntimeOperations } from "./internal"
import { tokens } from "./index"

describe("tokens", () => {
  test("creates a runtime handle and validates its completion", async () => {
    const calls: unknown[] = []
    let waitResult: unknown = { approved: true }
    const uninstall = installRuntimeOperations({
      waitFor: async () => {},
      waitUntil: async () => {},
      actorInputSend: async () => ({ sequence: 1 }),
      tokenCreate: async (options) => {
        calls.push({ operation: "create", options })
        return {
          id: "tok_aaaaaaaaaaaaaaaaaaaaaaaaaa",
          callbackUrl: "https://api.example.test/callback",
          publicAccessToken: "hlmr_pat_secret",
          timeoutAt: "2026-07-24T12:00:00Z",
          status: "pending",
          metadata: { approval: true },
          tags: ["review"],
          createdAt: "2026-07-24T11:50:00Z",
          updatedAt: "2026-07-24T11:50:00Z",
        }
      },
      tokenWait: async (tokenId, options) => {
        calls.push({ operation: "wait", tokenId, options })
        if (waitResult instanceof Error) throw waitResult
        return waitResult as never
      },
    })
    try {
      const token = await tokens.create({
        timeout: "10m",
        metadata: { approval: true },
        tags: ["review"],
        idempotencyKey: "approval-1",
      })
      const schema = {
        "~standard": {
          version: 1 as const,
          vendor: "test",
          validate(value: unknown) {
            return { value: (value as { approved: boolean }).approved }
          },
        },
      }
      await expect(token.wait({
        timeout: "30m",
        idleTimeout: "45s",
        metadata: { stage: "approval" },
        tags: ["human"],
        schema,
      }).unwrap()).resolves.toBe(true)
      waitResult = Object.assign(new Error("Token expired"), {
        code: "token_expired" as const,
        retryable: false as const,
      })
      const expired = await token.wait()
      expect(expired).toMatchObject({
        ok: false,
        error: { code: "token_expired", retryable: false },
      })
      expect(() => expired.unwrap()).toThrow("Token expired")
      waitResult = new DOMException("cancelled locally", "AbortError")
      await expect(Promise.resolve(token.wait())).rejects.toThrow("cancelled locally")
      expect(token.id).toBe("tok_aaaaaaaaaaaaaaaaaaaaaaaaaa")
      expect(calls).toEqual([
        {
          operation: "create",
          options: {
            timeout: "10m",
            metadata: { approval: true },
            tags: ["review"],
            idempotencyKey: "approval-1",
          },
        },
        {
          operation: "wait",
          tokenId: "tok_aaaaaaaaaaaaaaaaaaaaaaaaaa",
          options: {
            timeout: "30m",
            idleTimeout: "45s",
            metadata: { stage: "approval" },
            tags: ["human"],
            schema,
          },
        },
        {
          operation: "wait",
          tokenId: "tok_aaaaaaaaaaaaaaaaaaaaaaaaaa",
          options: {},
        },
        {
          operation: "wait",
          tokenId: "tok_aaaaaaaaaaaaaaaaaaaaaaaaaa",
          options: {},
        },
      ])
    } finally {
      uninstall()
    }
  })
})
