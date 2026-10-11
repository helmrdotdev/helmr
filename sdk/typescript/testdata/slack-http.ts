import assert from "node:assert/strict"
import { HelmrClient } from "../src/client"
const config = JSON.parse(await Bun.stdin.text())
const client = new HelmrClient({ url: config.url, apiKey: config.apiKey })
const request = { input: [{type: "text" as const, text: "route"}], slack: { channelId: config.channelId }, sessionKey: "slack-sdk", idempotencyKey: "start" }
const first = await client.agents.start("agent", request)
const replay = await client.agents.start("agent", request)
assert.equal(first.session.id, replay.session.id)
assert.equal(first.turn.id, replay.turn.id)
const next = await client.agents.start("agent", { input: [{type: "text", text: "continue"}], sessionKey: "slack-sdk", idempotencyKey: "continue" })
assert.equal(next.session.id, first.session.id)
assert.equal((await first.session.retrieve()).slackChannelId, config.channelId)
await assert.rejects(client.agents.start("agent", { input: [], slack: null } as never))
await assert.rejects(client.agents.start("agent", { input: [], slack: {} } as never))
await assert.rejects(client.agents.start("agent", { input: [], slack: { channelId: config.channelId, extra: true } } as never))
