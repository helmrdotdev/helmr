import { describe, expect, test } from "bun:test"
import { createHmac } from "node:crypto"
import { HumanRequests } from "../tasks/issue-fixer/human"
import { mkdtemp, readFile, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { acceptSlackEvent, projectSlackStatus } from "../interfaces/issue-fixer-slack"
import type { JsonValue, SessionRef } from "@helmr/sdk"

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (error: Error) => void
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}
function inbox(writeExtra: (value: any) => Promise<unknown> = async () => {}) {
  const stop = new AbortController()
  const native = new AbortController()
  const published: any[] = []
  const human = new HumanRequests(async value => { published.push(value); await writeExtra(value) }, stop.signal)
  return { human, stop, native, published }
}

describe("live native permission boundary", () => {
  test("out-of-order replies correlate independently; duplicate and wrong-kind replies reject", async () => {
    const { human, native, published } = inbox()
    const first = human.ask("approval", { nativeId: 1, command: "npm test" }, native.signal)
    const second = human.ask("answer", { nativeId: 2 }, native.signal, ["language"])
    const [a, b] = published
    await expect(human.reply({ type: "approval", requestId: b.requestId, allow: true })).rejects.toThrow("different reply kind")
    await expect(human.reply({ type: "answer", requestId: b.requestId, answers: { wrong: ["TS"] } })).rejects.toThrow("exact key")
    await human.reply({ type: "answer", requestId: b.requestId, answers: { language: ["TS"] } })
    expect((await second).type).toBe("answer")
    await human.reply({ type: "approval", requestId: a.requestId, allow: true })
    expect(await first).toEqual({ type: "approval", requestId: a.requestId, allow: true })
    expect(published[2]).toEqual({ type: "permission_admitted", requestId: a.requestId, actionBinding: a.actionBinding })
    await expect(human.reply({ type: "approval", requestId: a.requestId, allow: true })).rejects.toThrow("stale")
  })

  test("stop-first write rejection cannot grant even before local abort arrives", async () => {
    const { human, native, published, stop } = inbox(async value => {
      if (value.type === "permission_admitted") throw new Error("turn_stopping")
    })
    const answer = human.ask("approval", { command: "delete" }, native.signal)
    const settled = answer.catch(error => error)
    await expect(human.reply({ type: "approval", requestId: published[0].requestId, allow: true })).rejects.toThrow("turn_stopping")
    expect(await settled).toBeInstanceOf(Error)
    expect(stop.signal.aborted).toBe(false)
  })

  test("ambiguous admission write fails closed", async () => {
    const { human, native, published } = inbox(async value => {
      if (value.type === "permission_admitted") throw new Error("connection lost after append")
    })
    const answer = human.ask("approval", { command: "publish" }, native.signal)
    const settled = answer.catch(error => error)
    await expect(human.reply({ type: "approval", requestId: published[0].requestId, allow: true })).rejects.toThrow("connection lost")
    expect(await settled).toBeInstanceOf(Error)
  })

  test("native cancellation while fresh output is in flight invalidates the allow", async () => {
    const gate = deferred<void>()
    const { human, native, published } = inbox(value => value.type === "permission_admitted" ? gate.promise : Promise.resolve())
    const answer = human.ask("approval", { command: "write" }, native.signal)
    const settled = answer.catch(error => error)
    const sending = human.reply({ type: "approval", requestId: published[0].requestId, allow: true })
    native.abort()
    gate.resolve()
    await expect(sending).rejects.toThrow("cancelled")
    expect(await settled).toBeInstanceOf(Error)
  })

  test("a later provider request cannot adopt an earlier receipt or reply", async () => {
    const { human, native, published } = inbox()
    const a = human.ask("approval", { nativeId: 1 }, native.signal)
    await human.reply({ type: "approval", requestId: published[0].requestId, allow: false })
    await a
    const b = human.ask("approval", { nativeId: 1 }, native.signal)
    expect(published[1].requestId).not.toBe(published[0].requestId)
    await expect(human.reply({ type: "approval", requestId: published[0].requestId, allow: true })).rejects.toThrow("stale")
    await human.reply({ type: "approval", requestId: published[1].requestId, allow: false })
    await b
    expect(published.some(event => event.type === "permission_admitted")).toBe(false)
  })
})

function signedEvent(text: string, user = "U1") {
  const raw = Buffer.from(JSON.stringify({ type: "event_callback", api_app_id: "A1", team_id: "T1", event_id: "Ev1", event: { type: "app_mention", user, channel: "C1", text } }))
  const timestamp = String(Math.floor(Date.now() / 1000))
  const signature = `v0=${createHmac("sha256", "test-secret").update(`v0:${timestamp}:`).update(raw).digest("hex")}`
  return { raw, headers: new Headers({ "x-slack-request-timestamp": timestamp, "x-slack-signature": signature }) }
}

test("signed interface maps enqueue and exact replies without automatic retargeting", async () => {
  const calls: unknown[] = []
  const session = {
    enqueue: async (...args: unknown[]) => { calls.push(args.slice(0, 2)) },
    turn: (id: string) => ({ send: async (data: JsonValue, request: unknown) => { calls.push([id, data, request]); throw new Error("turn_not_active") } }),
  } as unknown as SessionRef
  const binding = { signingSecret: "test-secret", appId: "A1", teamId: "T1", channelId: "C1", users: ["U1"], session }
  const first = signedEvent("fix issue B")
  expect((await acceptSlackEvent(first.raw, first.headers, binding)).status).toBe(200)
  expect((await acceptSlackEvent(first.raw, first.headers, binding)).status).toBe(200)
  expect(calls[0]).toEqual(calls[1]) // Remote idempotency deduplicates this same key.
  const late = signedEvent('reply turn-A {"type":"approval","requestId":"old","allow":true}')
  expect((await acceptSlackEvent(late.raw, late.headers, binding)).status).toBe(503)
  expect(calls[2]).toEqual(["turn-A", { type: "approval", requestId: "old", allow: true }, { idempotencyKey: "slack:Ev1" }])
  const escaped = signedEvent('reply turn-A {"type":"answer","requestId":"question","answers":{"Use A &amp; B &lt; C &gt; D?":["yes &amp; no"]}}')
  await acceptSlackEvent(escaped.raw, escaped.headers, binding)
  expect(calls[3]).toEqual(["turn-A", { type: "answer", requestId: "question", answers: { "Use A & B < C > D?": ["yes & no"] } }, { idempotencyKey: "slack:Ev1" }])
  const unauthorized = signedEvent("fix issue C", "U2")
  expect((await acceptSlackEvent(unauthorized.raw, unauthorized.headers, binding)).status).toBe(403)
  first.headers.set("x-slack-request-timestamp", "1")
  expect((await acceptSlackEvent(first.raw, first.headers, binding)).status).toBe(401)
  expect(calls).toHaveLength(4)
})


test("uncertain Slack update retains cursor and repeats the same fixed destination", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-issue-projection-"))
  const previousFetch = globalThis.fetch
  const updates: string[] = []
  const cursors: number[] = []
  const session = { events: { list: async ({ after }: { after: number }) => {
    cursors.push(after)
    return { records: after === 0 ? [{ kind: "turn.completed", turnId: "A", data: {} }] : [], nextAfter: 1 }
  } } } as unknown as SessionRef
  globalThis.fetch = (async (_url: unknown, init: RequestInit) => {
    updates.push(String(init.body))
    if (updates.length === 1) throw new Error("Remote update succeeded but response was lost")
    return Response.json({ ok: true })
  }) as typeof fetch
  const options = { session, statePath: join(directory, "state.json"), botToken: "test", channel: "C1", messageTS: "123.456" }
  try {
    await expect(projectSlackStatus(options)).rejects.toThrow("response was lost")
    await projectSlackStatus(options)
    await projectSlackStatus(options)
    expect(updates).toHaveLength(2)
    expect(updates[0]).toEqual(updates[1])
    expect(cursors).toEqual([0, 0, 1])
    expect(JSON.parse(await readFile(options.statePath, "utf8")).after).toBe(1)
  } finally {
    globalThis.fetch = previousFetch
    await rm(directory, { recursive: true })
  }
})
