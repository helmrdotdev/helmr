import assert from "node:assert/strict"
import { HelmrClient } from "../src/client"
const config = JSON.parse(await Bun.stdin.text())
const client = new HelmrClient({ url: config.url, apiKey: config.apiKey })
const request = { input: [{type: "text" as const, text: "route"}], slack: { channelId: config.channelId }, key: "slack-sdk", idempotencyKey: "start" }
const first = await client.sessions.create({ agent: "agent", ...request })
const replay = await client.sessions.create({ agent: "agent", ...request })
assert.equal(first.session.id, replay.session.id)
assert.equal(first.turn.id, replay.turn.id)
await assert.rejects(client.sessions.create({agent:"agent",input:[{type:"text",text:"continue"}],key:"slack-sdk",idempotencyKey:"continue"}), {code:"idempotency_conflict"})
const next = await first.session.enqueue([{type:"text",text:"continue"}], {idempotencyKey:"continue"})
assert.equal(next.sessionId, first.session.id)
assert.equal((await first.session.get()).slackChannelId, config.channelId)
await assert.rejects(client.sessions.create({ agent: "agent", ...{ input: [], slack: null } as never }))
await assert.rejects(client.sessions.create({ agent: "agent", ...{ input: [], slack: {} } as never }))
await assert.rejects(client.sessions.create({ agent: "agent", ...{ input: [], slack: { channelId: config.channelId, extra: true } } as never }))
