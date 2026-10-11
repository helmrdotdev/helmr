import assert from "node:assert/strict"
import { HelmrClient } from "../src/index"
const config = await Bun.stdin.json() as { url: string; apiKey: string; sessionId: string; turnId: string; askId: string }
const client = new HelmrClient({ url: config.url, apiKey: config.apiKey })
const asks = client.sessions.get(config.sessionId).turn(config.turnId).asks
const first = await asks.list({ limit: 1 })
assert.equal(first.asks.length, 1)
assert.ok(first.nextCursor)
const second = await asks.list({ limit: 1, cursor: first.nextCursor })
assert.equal(second.asks.length, 1)
assert.notEqual(second.asks[0]!.id, first.asks[0]!.id)
const pending = await asks.get(config.askId)
assert.equal(pending.status, "pending")
assert.deepEqual(pending.prompt, [{ type: "text", text: "keep\0雪" }])
assert.deepEqual(pending.answerControl, { type: "text" })
const request = { answer: "Approved 雪\0", responseId: "sdk-answer" }
const receipt = await asks.respond(config.askId, request)
assert.equal(receipt.status, "responded")
assert.ok(receipt.respondedByApiKeyId)
assert.deepEqual(receipt.answer, request.answer)
assert.equal(Object.hasOwn(receipt, "prompt"), false)
assert.equal(Object.hasOwn(receipt, "answerControl"), false)
assert.deepEqual(await asks.respond(config.askId, request), receipt)
const final = await asks.get(config.askId)
assert.deepEqual(final.answer, request.answer)
assert.equal(final.respondedByApiKeyId, receipt.respondedByApiKeyId)
