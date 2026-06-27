import { createHmac, timingSafeEqual } from "node:crypto"
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http"
import { HelmrClient, type Task } from "@helmr/sdk"

export type Config = {
  readonly helmrUrl: string
  readonly helmrApiKey: string
  readonly taskId: string
  readonly port: number
  readonly slackBotToken: string
  readonly slackSigningSecret: string
  readonly slackChannelId: string
  readonly release: string
  readonly title: string
  readonly summary: string
  readonly risk?: string
  readonly stagingUrl?: string
  readonly productionUrl?: string
  readonly startIdempotencyKey?: string
  readonly sessionExternalId?: string
}

type SlackAction = {
  readonly sessionExternalId: string
  readonly approved: boolean
  readonly userId: string
  readonly channelId?: string
  readonly actionTs?: string
}

type ReleaseApprovalPayload = {
  readonly release: string
  readonly summary: string
  readonly approvalCorrelationId: string
  readonly risk?: string
  readonly stagingUrl?: string
  readonly productionUrl?: string
}

type ReleaseApprovalTask = Task<ReleaseApprovalPayload>

type BridgeDeps = {
  readonly sendApprovalInput: (action: SlackAction, data: unknown) => Promise<void>
}

type ApprovalTarget = {
  readonly sessionExternalId: string
}

if (import.meta.main) {
  const config = readConfig()
  const client = new HelmrClient({ url: config.helmrUrl, apiKey: config.helmrApiKey })
  startBridge(config, client).catch((error: unknown) => {
    console.error(error)
    process.exitCode = 1
  })
}

export async function startBridge(config: Config, client: HelmrClient): Promise<void> {
  const server = createServer((request, response) => {
    void route(request, response, config, {
      sendApprovalInput: async (action, data) => {
        await client.sessions.open({ externalId: action.sessionExternalId }).input("approval").send(data, {
          correlationId: action.sessionExternalId,
          idempotencyKey: approvalDecisionIdempotencyKey(action.sessionExternalId),
        })
      },
    }).catch((error: unknown) => {
      console.error(error)
      if (response.headersSent || response.writableEnded) return
      sendText(response, 500, "internal error")
    })
  })
  await listenBridge(server, config.port)
  console.log(`Slack approval bridge listening on http://localhost:${config.port}`)

  try {
    const sessionExternalId = approvalSessionExternalId(config)
    const started = await client.sessions.start<ReleaseApprovalTask>(config.taskId, {
      release: config.release,
      summary: config.summary,
      approvalCorrelationId: sessionExternalId,
      ...(config.risk === undefined ? {} : { risk: config.risk }),
      ...(config.stagingUrl === undefined ? {} : { stagingUrl: config.stagingUrl }),
      ...(config.productionUrl === undefined ? {} : { productionUrl: config.productionUrl }),
    }, {
      externalId: sessionExternalId,
      idempotencyKey: startIdempotencyKey(config),
      metadata: approvalMetadata(config),
      tags: ["approval", "bridge:slack-approval", "medium:slack"],
    })

    try {
      await postSlackApproval(config, { sessionExternalId })
    } catch (error) {
      await client.sessions.open(started.session).cancel({ reason: "slack_setup_failed" }).catch((cancelError: unknown) => {
        console.error("failed to cancel Slack approval session after setup failure", cancelError)
      })
      throw error
    }

    console.log(`started Helmr session ${started.session.id} for Slack approval ${sessionExternalId}`)
  } catch (error) {
    server.close()
    throw error
  }
}

export async function route(
  request: IncomingMessage,
  response: ServerResponse,
  config: Config,
  deps: BridgeDeps,
): Promise<void> {
  const url = new URL(request.url ?? "/", `http://localhost:${config.port}`)
  if (request.method === "GET" && url.pathname === "/health") {
    sendJson(response, 200, { ok: true })
    return
  }
  if (request.method === "POST" && url.pathname === "/slack/actions") {
    await handleSlackAction(request, response, config, deps)
    return
  }
  sendText(response, 404, "not found")
}

export async function handleSlackAction(
  request: IncomingMessage,
  response: ServerResponse,
  config: Config,
  deps: BridgeDeps,
): Promise<void> {
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

  let action: SlackAction
  try {
    action = parseSlackAction(payloadJson)
  } catch {
    sendText(response, 400, "invalid slack payload")
    return
  }
  const completion = {
    approved: action.approved,
    actor: action.userId,
    channelId: action.channelId,
    actionTs: action.actionTs,
  }
  try {
    await deps.sendApprovalInput(action, completion)
    sendJson(response, 200, {
      response_type: "ephemeral",
      text: action.approved ? "Approved in Helmr." : "Rejected in Helmr.",
    })
  } catch (error) {
    if (!isRecordedApprovalInput(error)) throw error
    sendJson(response, 200, {
      response_type: "ephemeral",
      text: "This approval was already recorded in Helmr.",
    })
  }
}

export async function postSlackApproval(config: Config, target: ApprovalTarget): Promise<void> {
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
            slackButton(target, "Approve", "primary", true),
            slackButton(target, "Reject", "danger", false),
          ],
        },
      ],
    }),
  })
  const result = await response.json() as { ok?: boolean; error?: string }
  if (!response.ok || result.ok !== true) throw new Error(`Slack post failed: ${result.error ?? response.statusText}`)
}

export function slackButton(target: ApprovalTarget, text: string, style: "primary" | "danger", approved: boolean) {
  return {
    type: "button",
    text: { type: "plain_text", text },
    style,
    action_id: approved ? "helmr_approve" : "helmr_reject",
    value: JSON.stringify({
      sessionExternalId: target.sessionExternalId,
      approved,
    }),
  }
}

export function verifySlackSignature(request: Pick<IncomingMessage, "headers">, rawBody: string, signingSecret: string): boolean {
  const timestamp = request.headers["x-slack-request-timestamp"]
  const signature = request.headers["x-slack-signature"]
  if (typeof timestamp !== "string" || typeof signature !== "string") return false
  const timestampSeconds = Number(timestamp)
  if (!Number.isFinite(timestampSeconds)) return false
  if (Math.abs(Date.now() / 1000 - timestampSeconds) > 60 * 5) return false

  const expected = slackSignature(rawBody, signingSecret, timestamp)
  const expectedBytes = Buffer.from(expected)
  const signatureBytes = Buffer.from(signature)
  return expectedBytes.length === signatureBytes.length && timingSafeEqual(expectedBytes, signatureBytes)
}

export function slackSignature(rawBody: string, signingSecret: string, timestamp: string): string {
  return `v0=${createHmac("sha256", signingSecret).update(`v0:${timestamp}:${rawBody}`).digest("hex")}`
}

export function parseSlackAction(payloadJson: string): SlackAction {
  const payload = JSON.parse(payloadJson) as Record<string, unknown>
  const actions = payload.actions
  if (!Array.isArray(actions) || actions.length === 0) throw new Error("missing Slack action")
  const action = actions[0] as Record<string, unknown>
  const value = JSON.parse(String(action.value)) as Record<string, unknown>
  const user = payload.user as Record<string, unknown> | undefined
  const channel = payload.channel as Record<string, unknown> | undefined
  const sessionExternalId = typeof value.sessionExternalId === "string" ? value.sessionExternalId.trim() : ""
  if (sessionExternalId.length === 0) throw new Error("missing session external id")
  if (typeof value.approved !== "boolean") throw new Error("missing approval decision")
  return {
    sessionExternalId,
    approved: value.approved,
    userId: String(user?.id ?? "slack-user"),
    channelId: typeof channel?.id === "string" ? channel.id : undefined,
    actionTs: typeof action.action_ts === "string" ? action.action_ts : undefined,
  }
}

export function isRecordedApprovalInput(error: unknown): boolean {
  if (error === null || typeof error !== "object") return false
  const code = "code" in error ? String((error as { readonly code?: unknown }).code) : ""
  if (code === "idempotency_fingerprint_mismatch") return true
  const message = error instanceof Error ? error.message : String(error)
  return message.includes("idempotency_fingerprint_mismatch")
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
    taskId: optionalEnv("HELMR_TASK_ID", "slack-approval"),
    port: numberEnv("PORT", 8787),
    slackBotToken: requiredEnv("SLACK_BOT_TOKEN"),
    slackSigningSecret: requiredEnv("SLACK_SIGNING_SECRET"),
    slackChannelId: requiredEnv("SLACK_CHANNEL_ID"),
    release: optionalEnv("APPROVAL_RELEASE", "release"),
    title: optionalEnv("APPROVAL_TITLE", "Approve release?"),
    summary: optionalEnv("APPROVAL_SUMMARY", "Review the release request and choose an action."),
    risk: optionalEnvOrUndefined("APPROVAL_RISK"),
    stagingUrl: optionalEnvOrUndefined("APPROVAL_STAGING_URL"),
    productionUrl: optionalEnvOrUndefined("APPROVAL_PRODUCTION_URL"),
    startIdempotencyKey: optionalEnvOrUndefined("HELMR_START_IDEMPOTENCY_KEY"),
    sessionExternalId: optionalEnvOrUndefined("HELMR_SESSION_EXTERNAL_ID"),
  }
}

function approvalMetadata(config: Config): Record<string, unknown> {
  return {
    release: config.release,
    summary: config.summary,
    title: config.title,
    risk: config.risk ?? null,
    stagingUrl: config.stagingUrl ?? null,
    productionUrl: config.productionUrl ?? null,
  }
}

function approvalSessionExternalId(config: Config): string {
  return config.sessionExternalId ?? `slack:${config.slackChannelId}:${config.release}`
}

function startIdempotencyKey(config: Config): string {
  return config.startIdempotencyKey ?? `slack-approval:${config.taskId}:${config.slackChannelId}:${config.release}:start`
}

function approvalDecisionIdempotencyKey(sessionExternalId: string): string {
  return `slack-approval:${sessionExternalId}:decision`
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

function optionalEnvOrUndefined(name: string): string | undefined {
  const value = process.env[name]
  return value === undefined || value.length === 0 ? undefined : value
}

function numberEnv(name: string, fallback: number): number {
  const value = process.env[name]
  if (value === undefined || value.length === 0) return fallback
  const parsed = Number(value)
  if (!Number.isFinite(parsed)) throw new Error(`${name} must be a number`)
  return parsed
}

async function listenBridge(server: Server, port: number): Promise<void> {
  await new Promise<void>((resolve, reject) => {
    const onError = (error: Error) => {
      server.off("listening", onListening)
      reject(error)
    }
    const onListening = () => {
      server.off("error", onError)
      resolve()
    }
    server.once("error", onError)
    server.once("listening", onListening)
    server.listen(port)
  })
}

function sendJson(response: ServerResponse, status: number, body: unknown): void {
  response.writeHead(status, { "content-type": "application/json" })
  response.end(JSON.stringify(body))
}

function sendText(response: ServerResponse, status: number, body: string): void {
  response.writeHead(status, { "content-type": "text/plain; charset=utf-8" })
  response.end(body)
}
