import { describe, expect, test } from "bun:test"
import {
  HelmrClient,
  MessageRejected,
} from "./index"
import {
  parseSessionEvent,
  parseTurnState,
} from "./internal"

const sessionId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33"
const turnId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34"
const holdId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35"
const operationId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36"
const messageId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37"
const createdAt = "2026-07-24T11:50:00Z"
const turnState = {
  id: turnId,
  session_id: sessionId,
  sequence: 1,
  input: [],
  source: { type: "external" },
  status: "completed",
  created_at: createdAt,
  interrupt_requested: false,
  accepts_messages: false,
  terminal_at: createdAt,
  completion_save_id: holdId,
}
function transport(responses: unknown[]) {
  const requests: Array<{ url: string; init?: RequestInit }> = []
  const client = new HelmrClient({
    url: "https://api.example.test",
    apiKey: "key",
    fetch: (async (input, init) => {
      requests.push({
        url: String(input),
        ...(init === undefined ? {} : { init }),
      })
      const value = responses.shift()
      return value instanceof Response ? value : Response.json(value)
    }) as typeof fetch,
  })
  return { client, requests }
}

describe("Session lifecycle client", () => {
  test("Agent starts with an initial Turn and supports subsequent admission", async () => {
    const { client, requests } = transport([
      { session_id: sessionId, turn_id: turnId, sequence: 1, created: true },
      { session_id: sessionId, turn_id: messageId, sequence: 2 },
    ])
    const { session, turn, created } = await client.agents.start("operator", {
      input: [{ type: "text", text: "initial" }],
      computer: client.computers.ref(holdId),
      sessionKey: "thread:1",
      idempotencyKey: "start-1",
    })
    expect(turn.id).toBe(turnId)
    expect(created).toBe(true)
    expect(JSON.parse(String(requests[0]?.init?.body))).toEqual({
      input: [{ type: "text", text: "initial" }],
      computer_id: holdId,
      session_key: "thread:1",
      idempotency_key: "start-1",
    })
    expect((await session.enqueue([])).id).toBe(messageId)
    expect(requests.map((r) => new URL(r.url).pathname)).toEqual([
      "/v1/agents/operator/start",
      `/v1/sessions/${sessionId}/enqueue`,
    ])
  })
  test("send creates references for its fixed route and exact replies preserve message identity", async () => {
    const { client, requests } = transport([
      { id: operationId, kind: "enqueued", turn_id: turnId },
      {
        id: operationId,
        kind: "messaged",
        turn_id: turnId,
        message_id: messageId,
      },
      {
        id: operationId,
        turn_id: turnId,
        message_id: messageId,
        status: "accepted",
      },
    ])
    const ref = client.sessions.get(sessionId),
      signal = new AbortController().signal
    const first = await ref.send(
      [{ type: "text", text: "work" }],
      { idempotencyKey: "send-1" },
      { signal },
    )
    expect(first.kind).toBe("enqueued")
    expect(first.turn.id).toBe(turnId)
    expect(first.turn.sessionId).toBe(sessionId)
    const second = await ref.send([{ type: "text", text: "reply" }])
    expect(second).toMatchObject({
      kind: "messaged",
      turn: { id: turnId },
      message: { id: messageId, status: "accepted" },
    })
    expect(await first.turn.send([])).toEqual({
      id: messageId,
      status: "accepted",
    })
    expect(requests[2]?.url).toEndWith(`/turns/${turnId}/messages`)
    expect(JSON.parse(String(requests[0]?.init?.body))).toEqual({
      data: [{ type: "text", text: "work" }],
      idempotency_key: "send-1",
    })
    expect(requests[0]?.init?.signal).toBe(signal)
    const keys = requests
      .slice(1)
      .map((r) => JSON.parse(String(r.init?.body)).idempotency_key)
    expect(keys[0]).toMatch(/^[0-9a-f-]{36}$/)
    expect(keys[1]).not.toBe(keys[0])
  })
  test("events have one ordered cursor and explicit retention gaps", async () => {
    const event = {
      session_id: sessionId,
      turn_id: null,
      sequence: 8,
      created_at: createdAt,
      kind: "turn.output",
      data: null,
    }
    const { client, requests } = transport([
      { records: [event], next_after: 8, has_more: true, retained_after: 3 },
      Response.json(
        {
          error: {
            code: "cursor_expired",
            message: "expired",
            details: { retained_after: 3 },
          },
        },
        { status: 410 },
      ),
    ])
    const ref = client.sessions.get(sessionId)
    expect(await ref.events.list({ after: 7, limit: 1000 })).toEqual({
      records: [
        {
              sessionId,
          turnId: null,
          sequence: 8,
          createdAt,
          kind: "turn.output",
          data: null,
            },
      ],
      nextAfter: 8,
      hasMore: true,
      retainedAfter: 3,
    })
    expect(requests[0]?.url).toEndWith("/events?after=7&limit=1000")
    await expect(ref.events.list()).rejects.toMatchObject({
      code: "cursor_expired",
      details: { retained_after: 3 },
    })
    expect(requests[1]?.url).toEndWith("/events?after=0&limit=100")
    await expect(ref.events.list({ limit: 1001 })).rejects.toThrow("[1,1000]")
    await expect(
      ref.events.list({ after: Number.MAX_SAFE_INTEGER + 1 }),
    ).rejects.toThrow("safe integer")
    expect(requests).toHaveLength(2)
  })
  test("event pages preserve message lifecycle and unavailable delivery before later output", async () => {
    const kinds = ["message.admitted", "message.started", "message.delivered", "message.rejected", "slack.delivery_unavailable", "turn.output"]
    const records = kinds.map((kind, index) => ({
      session_id: sessionId,
      turn_id: kind === "slack.delivery_unavailable" ? null : turnId,
      sequence: index + 8,
      created_at: createdAt,
      kind,
      data: kind.startsWith("message.") ? { message_id: messageId, status: kind.slice(8) } : { reason: "delivery_unavailable" },
    }))
    const { client } = transport([{ records, next_after: 13, has_more: false, retained_after: 0 }])
    const page = await client.sessions.get(sessionId).events.list({ after: 7 })
    expect(page.records.map(event => event.kind)).toEqual(kinds)
    expect(page.records.map(event => event.data)).toEqual(records.map(event => event.data))
    expect(page.records.map(event => event.turnId)).toEqual(records.map(event => event.turn_id))
    expect(page.nextAfter).toBe(13)
  })
  test("Turn retrieval retains absent and explicit null results", async () => {
    const { client } = transport([turnState, { ...turnState, result: null }])
    const ref = client.sessions.get(sessionId).turn(turnId)
    expect(Object.hasOwn(await ref.retrieve(), "result")).toBe(false)
    expect(await ref.retrieve()).toMatchObject({
      result: null,
      terminalAt: createdAt,
      completionSaveId: holdId,
    })
    expect(() => parseTurnState({ ...turnState, result: undefined })).toThrow()
  })
  test("Turn retrieval exposes immutable application failure", async () => {
    const failed = { ...turnState, status: "failed", completion_save_id: undefined, error: { code: "application_error", message: "fixture" } }
    const { client } = transport([failed])
    expect(await client.sessions.get(sessionId).turn(turnId).retrieve()).toMatchObject({ status: "failed", error: failed.error })
    expect(() => parseTurnState({ ...failed, error: { code: "", message: "fixture" } })).toThrow()
    expect(() => parseTurnState({ ...turnState, error: failed.error })).toThrow()
  })
  test("held closing Sessions expose independent dispatch state", async () => {
    const { client } = transport([
      {
        id: sessionId,
        agent_id: holdId,
        root_session_id: sessionId,
        requester_session_id: null,
        initial_turn: { id: turnId, status: "queued" },
        parent_session_id: null,
        deployment_id: holdId,
        computer_id: holdId,
        status: "closing",
        created_at: createdAt,
        updated_at: createdAt,
        active_turn_id: null,
        holds: [{id:holdId,session_id:sessionId,scope:"local",reason:"recovery_required",created_at:createdAt}],
      },
    ])
    expect(await client.sessions.retrieve(sessionId)).toMatchObject({
      status: "closing",
      holds: [{ id:holdId,scope:"local",reason:"recovery_required" }],
    })
  })
  test("controls carry exact target identities", async () => {
    const receipt = {
      id: operationId,
      session_id: sessionId,
      status: "accepted",
    }
    const { client, requests } = transport([
      { ...receipt, hold_id: holdId },
      { ...receipt, hold_id: holdId },
      receipt,
      receipt,
    ])
    const ref = client.sessions.get(sessionId)
    expect(
      await ref.interrupt({ idempotencyKey: "stop-1" }),
    ).toMatchObject({ sessionId, holdId, status: "accepted" })
    await ref.resume({ holdId, idempotencyKey: "resume-1" })
    await ref.close({ idempotencyKey: "close-1" })
    await ref.cancel({ idempotencyKey: "cancel-1" })
    expect(requests.map((r) => JSON.parse(String(r.init?.body)))).toEqual([
      { idempotency_key: "stop-1" },
      { hold_id: holdId, idempotency_key: "resume-1" },
      { idempotency_key: "close-1" },
      { idempotency_key: "cancel-1" },
    ])
  })
  test("event output requires serializable data and fixed lifecycle kind", () => {
    const event = {
      id: operationId,
      session_id: sessionId,
      turn_id: null,
      sequence: 1,
      created_at: createdAt,
      kind: "turn.output",
    }
    expect(() => parseSessionEvent(event)).toThrow()
    expect(() =>
      parseSessionEvent({ ...event, data: null, kind: "application.event" }),
    ).toThrow("kind")
  })
})

test("MessageRejected is recognizable across SDK copies", () => {
  const rejection = new MessageRejected("not applicable", { reason: "stale" })
  expect(rejection).toBeInstanceOf(MessageRejected)
  expect(rejection.details).toEqual({ reason: "stale" })
  expect({ [Symbol.for("helmr.sdk.MessageRejected")]: true }).toBeInstanceOf(
    MessageRejected,
  )
  expect(new Error("uncertain")).not.toBeInstanceOf(MessageRejected)
})

test("process diagnostics are not exposed by the public client", () => {
  const { client, requests } = transport([])
  expect("logs" in client.sessions).toBe(false)
  expect("logStreams" in client.sessions).toBe(false)
  expect("preparationLogs" in client.computers).toBe(false)
  expect("preparationLogStreams" in client.computers).toBe(false)
  expect(requests).toHaveLength(0)
})
