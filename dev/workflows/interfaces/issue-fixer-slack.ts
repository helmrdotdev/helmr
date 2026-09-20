import { createHmac, timingSafeEqual } from "node:crypto"
import { readFile, rename, writeFile } from "node:fs/promises"
import { z } from "zod"
import type { SessionRef } from "@helmr/sdk"
import { replySchema } from "../tasks/issue-fixer/human"

export type SlackBinding = {
  signingSecret: string
  appId: string
  teamId: string
  channelId: string
  users: readonly string[]
  session: SessionRef
}
const envelope = z.object({
  type: z.literal("event_callback"), api_app_id: z.string(), team_id: z.string(), event_id: z.string(),
  event: z.object({ type: z.literal("app_mention"), user: z.string(), channel: z.string(), text: z.string() }),
})

// Mount behind an HTTP server with a 64 KiB body limit and a request-read timeout.
// The host supplies the original bytes: never JSON.parse before verification.
export async function acceptSlackEvent(raw: Buffer, headers: Headers, binding: SlackBinding): Promise<Response> {
  if (raw.length > 65_536) return new Response("Body too large", { status: 413 })
  const timestamp = headers.get("x-slack-request-timestamp") ?? ""
  const received = headers.get("x-slack-signature") ?? ""
  if (!/^\d+$/.test(timestamp) || Math.abs(Date.now() / 1000 - Number(timestamp)) > 300 || !/^v0=[a-f0-9]{64}$/.test(received)) return new Response("Unauthorized", { status: 401 })
  const expected = `v0=${createHmac("sha256", binding.signingSecret).update(`v0:${timestamp}:`).update(raw).digest("hex")}`
  if (!timingSafeEqual(Buffer.from(expected), Buffer.from(received))) return new Response("Unauthorized", { status: 401 })
  let value: unknown
  try { value = JSON.parse(raw.toString("utf8")) } catch { return new Response("Invalid JSON", { status: 400 }) }
  const verification = z.object({ type: z.literal("url_verification"), challenge: z.string() }).safeParse(value)
  if (verification.success) return Response.json({ challenge: verification.data.challenge })
  const parsed = envelope.safeParse(value)
  if (!parsed.success) return new Response("Ignored", { status: 200 })
  const event = parsed.data
  if (event.api_app_id !== binding.appId || event.team_id !== binding.teamId || event.event.channel !== binding.channelId || !binding.users.includes(event.event.user)) return new Response("Forbidden", { status: 403 })
  const text = event.event.text.replace(/^<@[^>]+>\s*/, "").trim()
    .replace(/&amp;|&lt;|&gt;/g, value => ({ "&amp;": "&", "&lt;": "<", "&gt;": ">" })[value]!)
  const request = { idempotencyKey: `slack:${event.event_id}` }
  // Leave time for the host to ACK within Slack's three-second deadline.
  // A timeout is uncertain: return non-2xx so Slack retries the SAME operation key.
  const transport = { signal: AbortSignal.timeout(1_800) }
  try {
    if (text.startsWith("fix ")) {
      await binding.session.enqueue({ issue: text.slice(4) }, request, transport)
    } else if (text.startsWith("send ")) {
      await binding.session.send({ issue: text.slice(5) }, request, transport)
    } else if (text.startsWith("reply ")) {
      const match = /^reply (\S+) ([\s\S]+)$/.exec(text)
      if (!match) return new Response("Invalid reply", { status: 400 })
      const reply = replySchema.safeParse(JSON.parse(match[2]))
      if (!reply.success) return new Response("Invalid reply", { status: 400 })
      await binding.session.turn(match[1]).send(reply.data, request, transport)
    } else if (/^stop \S+$/.test(text)) {
      await binding.session.turn(text.slice(5)).interrupt(request, transport)
    } else if (/^resume \S+$/.test(text)) {
      await binding.session.resume({ ...request, holdId: text.slice(7) }, transport)
    } else return new Response("Unknown command", { status: 400 })
    return new Response("Accepted", { status: 200 })
  } catch {
    // No automatic send fallback, new idempotency key, or implicit hold clearing.
    return new Response("Not acknowledged; retry the same event", { status: 503 })
  }
}

type Projection = { after: number; lines: string[] }

// One projector per Session/state file. The operator creates the status message once.
// chat.update to a fixed destination can safely repeat the same body after a crash.
export async function projectSlackStatus(options: {
  session: SessionRef; statePath: string; botToken: string; channel: string; messageTS: string
}): Promise<void> {
  let state: Projection
  try {
    state = z.object({ after: z.number().int().nonnegative(), lines: z.array(z.string()) }).parse(JSON.parse(await readFile(options.statePath, "utf8")))
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error
    state = { after: 0, lines: [] }
  }
  const page = await options.session.events.list({ after: state.after, limit: 100 })
  if (!page.records.length) return
  const lines = [...state.lines, ...page.records.map(event => `${event.kind} turn=${event.turnId ?? "none"} ${JSON.stringify(event.data)}`)].slice(-8)
  const response = await fetch("https://slack.com/api/chat.update", {
    method: "POST", signal: AbortSignal.timeout(5_000),
    headers: { authorization: `Bearer ${options.botToken}`, "content-type": "application/json" },
    body: JSON.stringify({ channel: options.channel, ts: options.messageTS, text: lines.join("\n").slice(-12_000), parse: "none", mrkdwn: false }),
  })
  const result = z.object({ ok: z.boolean() }).parse(await response.json())
  if (!response.ok || !result.ok) throw new Error("Slack update failed; retain cursor and retry")
  // Persist only after remote success; a failed write repeats an update, not a new post.
  await writeFile(`${options.statePath}.tmp`, JSON.stringify({ after: page.nextAfter, lines }), { mode: 0o600 })
  await rename(`${options.statePath}.tmp`, options.statePath)
}
