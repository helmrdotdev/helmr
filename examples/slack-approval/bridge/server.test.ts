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

  test("parses session input action payloads", () => {
    const action = parseSlackAction(JSON.stringify({
      user: { id: "U123" },
      channel: { id: "C123" },
      actions: [{
        action_ts: "1710000000.0001",
        value: JSON.stringify({
          sessionExternalId: "slack:C123:v1.2.3",
          publicAccessToken: "hlmr_pat_secret",
          approved: true,
        }),
      }],
    }))

    expect(action).toEqual({
      sessionExternalId: "slack:C123:v1.2.3",
      publicAccessToken: "hlmr_pat_secret",
      approved: true,
      userId: "U123",
      channelId: "C123",
      actionTs: "1710000000.0001",
    })
  })

  test("rejects malformed Slack action payloads before session input", async () => {
    const payload = slackPayload({
      sessionExternalId: "slack:C123:v1.2.3",
      publicAccessToken: "hlmr_pat_secret",
      approved: "false",
    })
    const body = new URLSearchParams({ payload }).toString()
    const response = new MockResponse()
    let sent = false

    await handleSlackAction(signedRequest(body), response as unknown as ServerResponse, config, {
      sendApprovalInput: async () => {
        sent = true
      },
    })

    expect(response.status).toBe(400)
    expect(response.body).toBe("invalid slack payload")
    expect(sent).toBe(false)
  })

  test("sends session input from Slack action", async () => {
    const payload = slackPayload({
      sessionExternalId: "slack:C123:v1.2.3",
      publicAccessToken: "hlmr_pat_secret",
      approved: true,
    })
    const body = new URLSearchParams({ payload }).toString()
    const response = new MockResponse()
    const calls: unknown[] = []

    await handleSlackAction(signedRequest(body), response as unknown as ServerResponse, config, {
      sendApprovalInput: async (action, data) => {
        calls.push({ action, data })
      },
    })

    expect(response.status).toBe(200)
    expect(calls).toEqual([{
      action: {
        sessionExternalId: "slack:C123:v1.2.3",
        publicAccessToken: "hlmr_pat_secret",
        approved: true,
        userId: "U123",
        channelId: "C123",
        actionTs: "1710000000.0001",
      },
      data: { approved: true, actor: "U123", channelId: "C123", actionTs: "1710000000.0001" },
    }])
    expect(JSON.parse(response.body).text).toBe("Approved in Helmr.")
  })

  test("treats recorded session input as deterministic duplicate UI", async () => {
    const payload = slackPayload({
      sessionExternalId: "slack:C123:v1.2.3",
      publicAccessToken: "hlmr_pat_secret",
      approved: false,
    })
    const body = new URLSearchParams({ payload }).toString()
    const response = new MockResponse()

    await handleSlackAction(signedRequest(body), response as unknown as ServerResponse, config, {
      sendApprovalInput: async () => {
        throw new Error("Helmr API 403: {\"code\":\"token_scope_denied\"}")
      },
    })

    expect(response.status).toBe(200)
    expect(JSON.parse(response.body).text).toBe("This approval was already recorded in Helmr.")
  })

  test("cancels the parked session when Slack post fails", async () => {
    const originalFetch = globalThis.fetch
    const calls: unknown[] = []
    globalThis.fetch = async () => Response.json({ ok: false, error: "channel_not_found" })
    try {
      await expect(startBridge({ ...config, port: 0 }, {
        sessions: {
          start: async (_taskId: string, payload: unknown, opts: unknown) => {
            calls.push({ op: "sessions.start", payload, opts })
            return { session: { id: "session-1", taskId: "slack-approval", currentRunId: "run-1" } }
          },
          open: (session: unknown) => ({
            cancel: async (opts: unknown) => {
              calls.push({ op: "sessions.cancel", session, opts })
            },
          }),
        },
        auth: {
          createPublicToken: async (opts: unknown) => {
            calls.push({ op: "auth.createPublicToken", opts })
            return { publicAccessToken: "hlmr_pat_secret" }
          },
        },
      } as never)).rejects.toThrow("Slack post failed: channel_not_found")
    } finally {
      globalThis.fetch = originalFetch
    }

    expect(calls).toEqual([
      {
        op: "sessions.start",
        payload: {
          release: "v1.2.3",
          summary: "Ship v1.2.3",
          approvalCorrelationId: "slack:C123:v1.2.3",
        },
        opts: {
          externalId: "slack:C123:v1.2.3",
          idempotencyKey: "slack-approval:slack-approval:C123:v1.2.3:start",
          metadata: {
            release: "v1.2.3",
            summary: "Ship v1.2.3",
            title: "Approve release?",
            risk: null,
            stagingUrl: null,
            productionUrl: null,
          },
          tags: ["approval", "bridge:slack-approval", "medium:slack"],
        },
      },
      {
        op: "auth.createPublicToken",
        opts: {
          scope: {
            type: "session.input.send",
            session: { externalId: "slack:C123:v1.2.3" },
            stream: "approval",
            correlationId: "slack:C123:v1.2.3",
          },
          maxUses: 1,
          expiresAt: expect.any(Date),
        },
      },
      {
        op: "sessions.cancel",
        session: { id: "session-1", taskId: "slack-approval", currentRunId: "run-1" },
        opts: { reason: "slack_setup_failed" },
      },
    ])
  })

  test("cancels the parked session when public token creation fails", async () => {
    const calls: unknown[] = []
    await expect(startBridge({ ...config, port: 0 }, {
      sessions: {
        start: async (_taskId: string, payload: unknown, opts: unknown) => {
          calls.push({ op: "sessions.start", payload, opts })
          return { session: { id: "session-1", taskId: "slack-approval", currentRunId: "run-1" } }
        },
        open: (session: unknown) => ({
          cancel: async (opts: unknown) => {
            calls.push({ op: "sessions.cancel", session, opts })
          },
        }),
      },
      auth: {
        createPublicToken: async (opts: unknown) => {
          calls.push({ op: "auth.createPublicToken", opts })
          throw new Error("scope denied")
        },
      },
    } as never)).rejects.toThrow("scope denied")

    expect(calls.map((call) => (call as { op: string }).op)).toEqual([
      "sessions.start",
      "auth.createPublicToken",
      "sessions.cancel",
    ])
  })
})

function slackPayload(value: {
  readonly sessionExternalId: string
  readonly publicAccessToken: string
  readonly approved: unknown
}): string {
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
