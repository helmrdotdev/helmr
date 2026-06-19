import { createHmac, timingSafeEqual } from "node:crypto"
import { createServer, type IncomingMessage, type ServerResponse } from "node:http"
import { HelmrClient } from "@helmr/sdk"

type Config = {
  readonly helmrUrl: string
  readonly helmrApiKey: string
  readonly sessionId: string
  readonly port: number
  readonly slackBotToken: string
  readonly slackSigningSecret: string
  readonly slackChannelId: string
  readonly title: string
  readonly summary: string
}

const config = readConfig()
const client = new HelmrClient({ url: config.helmrUrl, apiKey: config.helmrApiKey })

createServer((request, response) => {
  void route(request, response).catch((error: unknown) => {
    console.error(error)
    sendText(response, 500, "internal error")
  })
}).listen(config.port, () => {
  console.log(`Slack approval bridge listening on http://localhost:${config.port}`)
  console.log(`sending approval input to Helmr session ${config.sessionId}`)
  void postSlackApproval().catch((error: unknown) => {
    console.error("initial Slack post failed", error)
  })
})

async function route(request: IncomingMessage, response: ServerResponse): Promise<void> {
  const url = new URL(request.url ?? "/", `http://localhost:${config.port}`)
  if (request.method === "GET" && url.pathname === "/health") {
    sendJson(response, 200, { ok: true })
    return
  }
  if (request.method === "POST" && url.pathname === "/slack/actions") {
    await handleSlackAction(request, response)
    return
  }
  sendText(response, 404, "not found")
}

async function handleSlackAction(request: IncomingMessage, response: ServerResponse): Promise<void> {
  const rawBody = await readBody(request)
  if (!verifySlackSignature(request, rawBody, config.slackSigningSecret)) {
    sendText(response, 401, "invalid slack signature")
    return
  }

  const payloadJson = new URLSearchParams(rawBody).get("payload")
  if (payloadJson === null) {
    sendText(response, 400, "missing slack payload")
    return
  }

  const action = parseSlackAction(payloadJson)
  await client.sessions.open(action.sessionId).input("approval").send({
    approved: action.approved,
    actor: action.userId,
    channelId: action.channelId,
  }, {
    correlationId: action.channelId,
    externalEventId: action.actionTs,
  })

  sendJson(response, 200, {
    response_type: "ephemeral",
    text: action.approved ? "Approved in Helmr." : "Rejected in Helmr.",
  })
}

async function postSlackApproval(): Promise<void> {
  const response = await fetch("https://slack.com/api/chat.postMessage", {
    method: "POST",
    headers: {
      authorization: `Bearer ${config.slackBotToken}`,
      "content-type": "application/json; charset=utf-8",
    },
    body: JSON.stringify({
      channel: config.slackChannelId,
      text: config.title,
      blocks: [
        { type: "header", text: { type: "plain_text", text: config.title } },
        { type: "section", text: { type: "mrkdwn", text: config.summary } },
        {
          type: "actions",
          elements: [
            slackButton("Approve", "primary", true),
            slackButton("Reject", "danger", false),
          ],
        },
      ],
    }),
  })
  const result = await response.json() as { ok?: boolean; error?: string }
  if (!response.ok || result.ok !== true) throw new Error(`Slack post failed: ${result.error ?? response.statusText}`)
}

function slackButton(text: string, style: "primary" | "danger", approved: boolean) {
  return {
    type: "button",
    text: { type: "plain_text", text },
    style,
    action_id: approved ? "helmr_approve" : "helmr_reject",
    value: JSON.stringify({
      sessionId: config.sessionId,
      approved,
    }),
  }
}

function verifySlackSignature(request: IncomingMessage, rawBody: string, signingSecret: string): boolean {
  const timestamp = request.headers["x-slack-request-timestamp"]
  const signature = request.headers["x-slack-signature"]
  if (typeof timestamp !== "string" || typeof signature !== "string") return false
  const timestampSeconds = Number(timestamp)
  if (!Number.isFinite(timestampSeconds)) return false
  if (Math.abs(Date.now() / 1000 - timestampSeconds) > 60 * 5) return false

  const expected = `v0=${createHmac("sha256", signingSecret).update(`v0:${timestamp}:${rawBody}`).digest("hex")}`
  const expectedBytes = Buffer.from(expected)
  const signatureBytes = Buffer.from(signature)
  return expectedBytes.length === signatureBytes.length && timingSafeEqual(expectedBytes, signatureBytes)
}

function parseSlackAction(payloadJson: string): {
  readonly sessionId: string
  readonly approved: boolean
  readonly userId: string
  readonly channelId?: string
  readonly actionTs?: string
} {
  const payload = JSON.parse(payloadJson) as Record<string, unknown>
  const actions = payload.actions
  if (!Array.isArray(actions) || actions.length === 0) throw new Error("missing Slack action")
  const action = actions[0] as Record<string, unknown>
  const value = JSON.parse(String(action.value)) as Record<string, unknown>
  const user = payload.user as Record<string, unknown> | undefined
  const channel = payload.channel as Record<string, unknown> | undefined
  return {
    sessionId: String(value.sessionId),
    approved: value.approved === true,
    userId: String(user?.id ?? "slack-user"),
    channelId: typeof channel?.id === "string" ? channel.id : undefined,
    actionTs: typeof action.action_ts === "string" ? action.action_ts : undefined,
  }
}

async function readBody(request: IncomingMessage): Promise<string> {
  const chunks: Buffer[] = []
  for await (const chunk of request) chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk))
  return Buffer.concat(chunks).toString("utf8")
}

function readConfig(): Config {
  return {
    helmrUrl: requiredEnv("HELMR_API_URL"),
    helmrApiKey: requiredEnv("HELMR_API_KEY"),
    sessionId: requiredEnv("HELMR_SESSION_ID"),
    port: numberEnv("PORT", 8787),
    slackBotToken: requiredEnv("SLACK_BOT_TOKEN"),
    slackSigningSecret: requiredEnv("SLACK_SIGNING_SECRET"),
    slackChannelId: requiredEnv("SLACK_CHANNEL_ID"),
    title: optionalEnv("APPROVAL_TITLE", "Approve release?"),
    summary: optionalEnv("APPROVAL_SUMMARY", "Review the release request and choose an action."),
  }
}

function requiredEnv(name: string): string {
  const value = process.env[name]
  if (value === undefined || value.length === 0) throw new Error(`${name} is required`)
  return value
}

function optionalEnv(name: string, fallback: string): string {
  const value = process.env[name]
  return value === undefined || value.length === 0 ? fallback : value
}

function numberEnv(name: string, fallback: number): number {
  const value = process.env[name]
  if (value === undefined || value.length === 0) return fallback
  const parsed = Number(value)
  if (!Number.isFinite(parsed)) throw new Error(`${name} must be a number`)
  return parsed
}

function sendJson(response: ServerResponse, status: number, body: unknown): void {
  response.writeHead(status, { "content-type": "application/json" })
  response.end(JSON.stringify(body))
}

function sendText(response: ServerResponse, status: number, body: string): void {
  response.writeHead(status, { "content-type": "text/plain; charset=utf-8" })
  response.end(body)
}
