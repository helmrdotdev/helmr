import { describe, expect, test } from "bun:test"

import { HelmrClient, task, workspaces } from "./index"
import {
  HELMR_API_VERSION,
  HELMR_API_VERSION_HEADER,
  HELMR_SDK_VERSION,
  HELMR_SDK_VERSION_HEADER,
} from "./version"

describe("HelmrClient Tasks", () => {
  test("starts a typed Task by runtime declared ID", async () => {
    const requests: Array<{ url: string; init?: RequestInit }> = []
    const resizeImage = task({
      id: "resize-image",
      payload: {
        "~standard": {
          version: 1,
          vendor: "test",
          validate(value: unknown) {
            return { value: value as { imageId: string } }
          },
        },
      },
      run: async (payload) => ({ resized: payload.imageId }),
    })
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async (input: URL | RequestInfo, init?: RequestInit) => {
        requests.push({ url: String(input), init })
        return Response.json({
          run_id: "run_aaaaaaaaaaaaaaaaaaaaaaaaaa",
        }, { status: 201 })
      }) as typeof fetch,
    })

    const run = await client.tasks.start<typeof resizeImage>("resize-image", {
      payload: { imageId: "image-1" },
      options: {
        workspace: workspaces.ref({
          id: "wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa",
        }),
        idempotencyKey: "image-1",
        concurrencyKey: "customer-1",
        retry: { maxAttempts: 3 },
        metadata: { source: "backend" },
        tags: ["image"],
      },
    })

    expect(run).toEqual({ id: "run_aaaaaaaaaaaaaaaaaaaaaaaaaa" })
    expect(requests[0]!.url).toBe(
      "https://api.example.test/api/tasks/resize-image/start",
    )
    expect(JSON.parse(String(requests[0]!.init?.body))).toEqual({
      payload: { imageId: "image-1" },
      options: {
        workspace: { id: "wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa" },
        idempotency_key: "image-1",
        concurrency_key: "customer-1",
        retry: { max_attempts: 3 },
        metadata: { source: "backend" },
        tags: ["image"],
      },
    })
  })
})

describe("HelmrClient Tokens", () => {
  test("creates an Environment-scoped Token through authenticated REST", async () => {
    const requests: Array<{ url: string; init?: RequestInit }> = []
    const client = new HelmrClient({
      url: "https://api.example.test",
      apiKey: "api-key",
      fetch: (async (input: URL | RequestInfo, init?: RequestInit) => {
        requests.push({ url: String(input), init })
        return Response.json({
          id: "tok_aaaaaaaaaaaaaaaaaaaaaaaaaa",
          status: "pending",
          callback_url: "https://api.example.test/api/token-callbacks/token/secret",
          public_access_token: "hlmr_pat_secret",
          timeout_at: "2026-07-24T12:00:00Z",
          metadata: { approval: true },
          tags: ["review"],
          created_at: "2026-07-24T11:50:00Z",
          updated_at: "2026-07-24T11:50:00Z",
        }, { status: 201 })
      }) as typeof fetch,
    })

    const token = await client.tokens.create({
      timeout: "10m",
      metadata: { approval: true },
      tags: ["review"],
      idempotencyKey: "approval-1",
    })

    expect(token.id).toBe("tok_aaaaaaaaaaaaaaaaaaaaaaaaaa")
    expect(requests).toHaveLength(1)
    expect(requests[0]!.url).toBe("https://api.example.test/api/tokens")
    expect(requests[0]!.init?.method).toBe("POST")
    expect(requests[0]!.init?.headers).toMatchObject({
      Authorization: "Bearer api-key",
      [HELMR_API_VERSION_HEADER]: HELMR_API_VERSION,
      [HELMR_SDK_VERSION_HEADER]: HELMR_SDK_VERSION,
    })
    expect(JSON.parse(String(requests[0]!.init?.body))).toEqual({
      timeout: "10m",
      metadata: { approval: true },
      tags: ["review"],
      idempotency_key: "approval-1",
    })
  })

  test.each([400, 401, 403, 409, 410])(
    "preserves structured Helmr errors for status %i",
    async (status) => {
      const client = new HelmrClient({
        url: "https://api.example.test",
        apiKey: "api-key",
        fetch: (async () => Response.json({
          error: `request failed with ${status}`,
          code: `token_error_${status}`,
          retryable: status === 409,
          request_id: `request-${status}`,
        }, { status })) as typeof fetch,
      })

      try {
        await client.tokens.retrieve("tok_aaaaaaaaaaaaaaaaaaaaaaaaaa")
        throw new Error("expected Token retrieve to fail")
      } catch (error) {
        expect(error).toMatchObject({
          name: "HelmrError",
          message: `request failed with ${status}`,
          code: `token_error_${status}`,
          retryable: status === 409,
          requestId: `request-${status}`,
        })
      }
    },
  )
})
