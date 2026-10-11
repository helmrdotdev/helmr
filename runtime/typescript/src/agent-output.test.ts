import assert from "node:assert/strict"
import test from "node:test"
import { AgentOutputWriter } from "./agent-output"
import { normalizeContent, type Content, type HumanContent } from "../../../sdk/typescript/src/content"

function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>(done => { resolve = done }); return { promise, resolve } }
const signal = () => new AbortController().signal
const cleanup = async (pending: Promise<unknown>) => { await pending }

test("pipe batches text exactly and flushes the received prefix before a question", async () => {
  const entered = deferred<void>(), continuation = deferred<void>()
  const calls: unknown[] = []
  const writer = new AgentOutputWriter(async content => { calls.push(content); return { sequence: calls.length } })
  async function* values() { yield "a"; yield " "; yield "b"; entered.resolve(); await continuation.promise; yield [{ type: "json", value: false }] as const; yield "c" }
  const pending = writer.pipe(values(), signal(), cleanup)
  await entered.promise
  await writer.ordered(async () => { calls.push("ask") })
  assert.deepEqual(calls, [[{ type: "text", text: "a b" }], "ask"])
  continuation.resolve()
  await pending
  assert.deepEqual(calls[2], [{ type: "json", value: false }, { type: "text", text: "c" }])
})

test("65 atomic JSON fragments flush within the part bound without truncation", async () => {
  const batches: Content[] = []
  const writer = new AgentOutputWriter(async content => { batches.push(content); return { sequence: batches.length } })
  async function* values() { for (let i = 0; i < 65; i++) yield [{ type: "json", value: i }] as const }
  await writer.pipe(values(), signal(), cleanup)
  assert.deepEqual(batches.map(batch => batch.length), [64, 1])
  assert.deepEqual(batches.flat(), Array.from({ length: 65 }, (_, value) => ({ type: "json", value })))
})

test("pipe timer flushes while the source is blocked and applies backpressure", async () => {
  const flushed = deferred<void>(), acknowledged = deferred<void>(), next = deferred<IteratorResult<HumanContent>>()
  let pulls = 0
  const source: AsyncIterable<HumanContent> = { [Symbol.asyncIterator]: () => ({ next: async () => ++pulls === 1 ? { done: false, value: "prefix" } : pulls === 2 ? next.promise : { done: true, value: undefined } }) }
  const writer = new AgentOutputWriter(async content => { assert.deepEqual(content, normalizeContent("prefix")); flushed.resolve(); await acknowledged.promise; return { sequence: 1 } })
  const pending = writer.pipe(source, signal(), cleanup)
  await flushed.promise
  next.resolve({ done: true, value: undefined })
  let settled = false
  void pending.then(() => { settled = true })
  await new Promise(resolve => setTimeout(resolve, 10))
  assert.equal(settled, false)
  assert.equal(pulls, 2)
  acknowledged.resolve()
  await pending
})

test("cancelled pipe retains only its durable prefix and closes the source", async () => {
  const cancel = new AbortController(), accepted = deferred<void>(), next = deferred<IteratorResult<HumanContent>>()
  let returned = 0, pulls = 0
  const batches: Content[] = []
  const source: AsyncIterable<HumanContent> = { [Symbol.asyncIterator]: () => ({
    next: async () => ++pulls === 1 ? { done: false, value: "x".repeat(16 * 1024) } : next.promise,
    return: async () => { returned++; return { done: true, value: undefined } },
  }) }
  const writer = new AgentOutputWriter(async content => { batches.push(content); accepted.resolve(); return { sequence: 1 } })
  const pending = writer.pipe(source, cancel.signal, cleanup)
  await accepted.promise
  cancel.abort(new Error("cancelled"))
  await assert.rejects(pending, /cancelled/)
  assert.equal(returned, 1)
  assert.equal(batches.length, 1)
})
