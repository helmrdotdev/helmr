import { describe, expect, test } from "bun:test"
import type { IncomingMessage, ServerResponse } from "node:http"
import {
  handleSlackAction,
  parseSlackAction,
  slackSignature,
  startBridge,
  verifySlackSignature,
  type Config,
} from "./server"

const config: Config = {
  helmrUrl: "http://127.0.0.1:3000",
  helmrApiKey: "test-key",
  taskId: "slack-approval",
  port: 8787,
  slackBotToken: "xoxb-test",
  slackSigningSecret: "secret",
  slackChannelId: "C123",
  release: "v1.2.3",
  title: "Approve release?",
  summary: "Ship v1.2.3",
}

describe("Slack approval bridge", () => {
  test("verifies Slack signatures", () => {
    const body = "payload=%7B%7D"
    const timestamp = String(Math.floor(Date.now() / 1000))
    const request = {
      headers: {
        "x-slack-request-timestamp": timestamp,
        "x-slack-signature": slackSignature(body, config.slackSigningSecret, timestamp),
      },
    } as Pick<IncomingMessage, "headers">

    expect(verifySlackSignature(request, body, config.slackSigningSecret)).toBe(true)
    expect(verifySlackSignature(request, `${body}x`, config.slackSigningSecret)).toBe(false)
  })

  test("parses token action payloads", () => {
    const action = parseSlackAction(JSON.stringify({
      user: { id: "U123" },
      channel: { id: "C123" },
      actions: [{
        action_ts: "1710000000.0001",
        value: JSON.stringify({ tokenId: "token-1", approved: true }),
      }],
    }))

    expect(action).toEqual({
      tokenId: "token-1",
      approved: true,
      userId: "U123",
      channelId: "C123",
      actionTs: "1710000000.0001",
    })
  })

  test("rejects malformed Slack action payloads before token completion", async () => {
    const payload = slackPayload({ tokenId: "token-1", approved: "false" })
    const body = new URLSearchParams({ payload }).toString()
    const response = new MockResponse()
    let completed = false

    await handleSlackAction(signedRequest(body), response as unknown as ServerResponse, config, {
      completeToken: async () => {
        completed = true
      },
    })

    expect(response.status).toBe(400)
    expect(response.body).toBe("invalid slack payload")
    expect(completed).toBe(false)
  })

  test("completes token from Slack action", async () => {
    const payload = slackPayload({ tokenId: "token-1", approved: true })
    const body = new URLSearchParams({ payload }).toString()
    const response = new MockResponse()
    const calls: unknown[] = []

    await handleSlackAction(signedRequest(body), response as unknown as ServerResponse, config, {
      completeToken: async (tokenId, data) => {
        calls.push({ tokenId, data })
      },
    })

    expect(response.status).toBe(200)
    expect(calls).toEqual([{
      tokenId: "token-1",
      data: { approved: true, actor: "U123", channelId: "C123" },
    }])
    expect(JSON.parse(response.body).text).toBe("Approved in Helmr.")
  })

  test("treats duplicate token completion as deterministic duplicate UI", async () => {
    const payload = slackPayload({ tokenId: "token-1", approved: false })
    const body = new URLSearchParams({ payload }).toString()
    const response = new MockResponse()

    await handleSlackAction(signedRequest(body), response as unknown as ServerResponse, config, {
      completeToken: async () => {
        throw new Error("Helmr API 409: {\"code\":\"token_completion_conflict\"}")
      },
    })

    expect(response.status).toBe(200)
    expect(JSON.parse(response.body).text).toBe("This approval was already recorded in Helmr.")
  })

  test("does not start a parked task when Slack post fails", async () => {
    const originalFetch = globalThis.fetch
    const calls: unknown[] = []
    globalThis.fetch = async () => Response.json({ ok: false, error: "channel_not_found" })
    try {
      await expect(startBridge({ ...config, port: 0 }, {
        tokens: {
          create: async (opts: unknown) => {
            calls.push({ op: "token.create", opts })
            return { id: "token-1" }
          },
          cancel: async (tokenId: string) => {
            calls.push({ op: "token.cancel", tokenId })
          },
          complete: async () => {},
        },
        tasks: {
          start: async () => {
            calls.push({ op: "task.start" })
            return { session: { id: "session-1" } }
          },
        },
      } as never)).rejects.toThrow("Slack post failed: channel_not_found")
    } finally {
      globalThis.fetch = originalFetch
    }

    expect(calls).toEqual([
      {
        op: "token.create",
        opts: {
          timeout: "7d",
          tags: ["approval", "bridge:slack-approval", "medium:slack"],
          metadata: {
            release: "v1.2.3",
            summary: "Ship v1.2.3",
            title: "Approve release?",
            risk: null,
            stagingUrl: null,
            productionUrl: null,
          },
          idempotencyKey: "slack-approval:slack-approval:C123:v1.2.3:token",
        },
      },
      { op: "token.cancel", tokenId: "token-1" },
    ])
  })
})

function slackPayload(value: { readonly tokenId: string; readonly approved: unknown }): string {
  return JSON.stringify({
    user: { id: "U123" },
    channel: { id: "C123" },
    actions: [{
      action_ts: "1710000000.0001",
      value: JSON.stringify(value),
    }],
  })
}

function signedRequest(body: string): IncomingMessage {
  const timestamp = String(Math.floor(Date.now() / 1000))
  const chunks = [Buffer.from(body)]
  return {
    headers: {
      "x-slack-request-timestamp": timestamp,
      "x-slack-signature": slackSignature(body, config.slackSigningSecret, timestamp),
    },
    async *[Symbol.asyncIterator]() {
      yield* chunks
    },
  } as unknown as IncomingMessage
}

class MockResponse {
  status = 0
  body = ""

  writeHead(status: number): void {
    this.status = status
  }

  end(body?: string): void {
    this.body = body ?? ""
  }
}
