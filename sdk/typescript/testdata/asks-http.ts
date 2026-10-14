import assert from "node:assert/strict"
import { HelmrClient } from "../src/index"
const config = await Bun.stdin.json() as { url: string; apiKey: string; sessionId: string; turnId: string; askId: string }
const client = new HelmrClient({ url: config.url, apiKey: config.apiKey })
const asks = client.sessions.ref(config.sessionId).turns.ref(config.turnId).asks
const first = await asks.list({ limit: 1 })
assert.equal(first.items.length, 1)
assert.ok(first.nextCursor)
const second = await asks.list({ limit: 1, cursor: first.nextCursor })
assert.equal(second.items.length, 1)
assert.notEqual(second.items[0]!.id, first.items[0]!.id)
const pending = await asks.get(config.askId)
assert.equal(pending.status, "pending")
assert.deepEqual(pending.question?.prompt, [{ type: "text", text: "keep\0雪" }])
assert.equal(pending.question?.type, "text")
const request = { answer: "Approved 雪\0", answerId: "sdk-answer" }
const receipt = await asks.answer(config.askId, request)
assert.equal(receipt.status, "answered")
assert.ok(receipt.answeredByApiKeyId)
assert.deepEqual(receipt.answer, request.answer)
assert.equal(Object.hasOwn(receipt, "question"), false)
assert.deepEqual(await asks.answer(config.askId, request), receipt)
const final = await asks.get(config.askId)
assert.deepEqual(final.answer, request.answer)
assert.equal(final.answeredByApiKeyId, receipt.answeredByApiKeyId)
