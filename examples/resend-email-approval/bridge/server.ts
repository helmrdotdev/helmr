import { randomUUID } from "node:crypto"
import { createServer, type IncomingMessage, type ServerResponse } from "node:http"
import { HelmrClient } from "@helmr/sdk"

type Config = {
  readonly helmrUrl: string
  readonly helmrApiKey: string
  readonly port: number
  readonly publicBaseUrl: string
  readonly pollIntervalMs: number
  readonly resendApiKey: string
  readonly emailFrom: string
  readonly emailTo: string
}

type PendingResponse = {
  readonly tokenId: string
  readonly approved: boolean
  readonly siblingId: string
  readonly expiresAt: number
}

const config = readConfig()
const client = new HelmrClient({ url: config.helmrUrl, apiKey: config.helmrApiKey })
type ClientToken = Awaited<ReturnType<typeof client.tokens.list>>[number]
const bridgeTag = "bridge:resend-email-approval"
const delivered = new Set<string>()
const pendingResponseTTLMS = 24 * 60 * 60 * 1000
// Demo bridge state is intentionally in-memory. Production bridges should
// persist pending response IDs when they run multiple replicas or need restart
// durability.
const pendingResponses = new Map<string, PendingResponse>()

createServer((request, response) => {
  void route(request, response).catch((error: unknown) => {
    console.error(error)
    sendText(response, 500, "internal error")
  })
}).listen(config.port, () => {
  console.log(`Resend email approval bridge listening on ${config.publicBaseUrl}`)
  console.log(`watching Helmr tokens tagged ${bridgeTag}`)
})

void pollLoop()

async function pollLoop(): Promise<void> {
  for (;;) {
    try {
      const requests = await client.tokens.list({ status: "pending", limit: 25 })
      await Promise.all(requests.map(deliverRequest))
    } catch (error) {
      console.error("poll failed", error)
    }
    await sleep(config.pollIntervalMs)
  }
}

async function deliverRequest(request: ClientToken): Promise<void> {
  if (!request.tags?.includes(bridgeTag)) return
  if (delivered.has(request.id)) return
  await sendApprovalEmail(request, request)
  delivered.add(request.id)
}

async function route(request: IncomingMessage, response: ServerResponse): Promise<void> {
  const url = new URL(request.url ?? "/", config.publicBaseUrl)
  if (request.method === "GET" && url.pathname === "/health") {
    sendJson(response, 200, { ok: true })
    return
  }
  if (request.method === "GET" && url.pathname === "/email/respond") {
    handleEmailResponsePage(url, response)
    return
  }
  if (request.method === "POST" && url.pathname === "/email/respond") {
    await handleEmailResponse(request, response)
    return
  }
  sendText(response, 404, "not found")
}

function handleEmailResponsePage(url: URL, response: ServerResponse): void {
  const responseId = url.searchParams.get("response_id")
  const pending = responseId === null ? undefined : pendingResponse(responseId)
  if (responseId === null || pending === undefined) {
    sendText(response, 400, "invalid response link")
    return
  }
  const action = pending.approved ? "Approve" : "Reject"
  sendHtml(response, 200, [
    `<h1>${action} release?</h1>`,
    "<p>This page has not recorded a response yet.</p>",
    `<form method="post" action="/email/respond">`,
    `<input type="hidden" name="response_id" value="${escapeHtml(responseId)}">`,
    `<button type="submit">${action}</button>`,
    `</form>`,
  ].join(""))
}

async function handleEmailResponse(request: IncomingMessage, response: ServerResponse): Promise<void> {
  const form = new URLSearchParams(await readBody(request))
  const responseId = form.get("response_id")
  const pending = responseId === null ? undefined : pendingResponse(responseId)
  if (responseId === null || pending === undefined) {
    sendText(response, 400, "invalid response form")
    return
  }

  const payload = {
    approved: pending.approved,
    bridge: "resend",
    actor: config.emailTo,
  }
  await client.tokens.complete(pending.tokenId, payload)
  deletePendingResponse(responseId, pending)
  sendHtml(response, 200, `<h1>Response recorded</h1><p>${payload.approved ? "Approved" : "Rejected"}.</p>`)
}

async function sendApprovalEmail(request: ClientToken, token: ClientToken): Promise<void> {
  const message = formatEmail(request, token)
  const response = await fetch("https://api.resend.com/emails", {
    method: "POST",
    headers: {
      authorization: `Bearer ${config.resendApiKey}`,
      "content-type": "application/json",
    },
    body: JSON.stringify({
      from: config.emailFrom,
      to: [config.emailTo],
      subject: message.subject,
      html: message.html,
      text: message.text,
      tags: [{ name: "category", value: "helmr_input" }],
    }),
  })
  if (!response.ok) {
    const body = await response.text()
    throw new Error(`Resend send failed: ${response.status} ${body}`)
  }
}

function formatEmail(request: ClientToken, token: ClientToken): {
  readonly subject: string
  readonly html: string
  readonly text: string
} {
  const details = requestDetails(request)
  const { approveUrl, rejectUrl } = responseUrls(token)
  return {
    subject: details.title,
    html: [
      `<h1>${escapeHtml(details.title)}</h1>`,
      `<p>${escapeHtml(details.summary)}</p>`,
      `<p><a href="${escapeHtml(approveUrl)}">Approve</a></p>`,
      `<p><a href="${escapeHtml(rejectUrl)}">Reject</a></p>`,
    ].join(""),
    text: [
      details.title,
      "",
      details.summary,
      "",
      `Approve confirmation page: ${approveUrl}`,
      `Reject confirmation page: ${rejectUrl}`,
    ].join("\n"),
  }
}

function responseUrls(token: ClientToken): { readonly approveUrl: string; readonly rejectUrl: string } {
  pruneExpiredPendingResponses()
  const approveId = randomUUID()
  const rejectId = randomUUID()
  const expiresAt = Date.now() + pendingResponseTTLMS
  pendingResponses.set(approveId, { tokenId: token.id, approved: true, siblingId: rejectId, expiresAt })
  pendingResponses.set(rejectId, { tokenId: token.id, approved: false, siblingId: approveId, expiresAt })
  return { approveUrl: responseUrl(approveId), rejectUrl: responseUrl(rejectId) }
}

function responseUrl(responseId: string): string {
  const url = new URL("/email/respond", config.publicBaseUrl)
  url.searchParams.set("response_id", responseId)
  return url.toString()
}

function pendingResponse(responseId: string): PendingResponse | undefined {
  const pending = pendingResponses.get(responseId)
  if (pending === undefined) return undefined
  if (pending.expiresAt <= Date.now()) {
    deletePendingResponse(responseId, pending)
    return undefined
  }
  return pending
}

function deletePendingResponse(responseId: string, pending: { readonly siblingId: string }): void {
  pendingResponses.delete(responseId)
  pendingResponses.delete(pending.siblingId)
}

function pruneExpiredPendingResponses(): void {
  const now = Date.now()
  for (const [responseId, pending] of pendingResponses) {
    if (pending.expiresAt <= now) deletePendingResponse(responseId, pending)
  }
}

function requestDetails(request: ClientToken): { readonly title: string; readonly summary: string } {
  const value = request.metadata
  const record = value !== null && typeof value === "object" ? value as Record<string, unknown> : {}
  const title = typeof record.release === "string" ? `Approve ${record.release}` : `Token ${request.id}`
  const lines = [
    field("Release", record.release),
    field("Summary", record.summary),
    field("Risk", record.risk),
    field("Staging", record.stagingUrl),
    field("Production", record.productionUrl),
  ].filter((line): line is string => line !== null)
  return { title, summary: lines.length === 0 ? `Token ${request.id}` : lines.join("\n") }
}

function field(label: string, value: unknown): string | null {
  return typeof value === "string" && value.length > 0 ? `${label}: ${value}` : null
}

function escapeHtml(value: string): string {
  return value
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
}

async function readBody(request: IncomingMessage): Promise<string> {
  const chunks: Buffer[] = []
  for await (const chunk of request) {
    chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk))
  }
  return Buffer.concat(chunks).toString("utf8")
}

function readConfig(): Config {
  const port = numberEnv("PORT", 8788)
  return {
    helmrUrl: requiredEnv("HELMR_API_URL"),
    helmrApiKey: requiredEnv("HELMR_API_KEY"),
    port,
    publicBaseUrl: requiredEnv("PUBLIC_BASE_URL"),
    pollIntervalMs: numberEnv("POLL_INTERVAL_MS", 2000),
    resendApiKey: requiredEnv("RESEND_API_KEY"),
    emailFrom: requiredEnv("RESEND_FROM"),
    emailTo: requiredEnv("EMAIL_TO"),
  }
}

function requiredEnv(name: string): string {
  const value = process.env[name]
  if (value === undefined || value.length === 0) throw new Error(`${name} is required`)
  return value
}

function numberEnv(name: string, fallback: number): number {
  const value = process.env[name]
  if (value === undefined || value.length === 0) return fallback
  const parsed = Number(value)
  if (!Number.isFinite(parsed)) throw new Error(`${name} must be a number`)
  return parsed
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

function sendJson(response: ServerResponse, status: number, body: unknown): void {
  response.writeHead(status, { "content-type": "application/json" })
  response.end(JSON.stringify(body))
}

function sendText(response: ServerResponse, status: number, body: string): void {
  response.writeHead(status, { "content-type": "text/plain; charset=utf-8" })
  response.end(body)
}

function sendHtml(response: ServerResponse, status: number, body: string): void {
  response.writeHead(status, { "content-type": "text/html; charset=utf-8" })
  response.end(`<!doctype html><html><body>${body}</body></html>`)
}
