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
  test("Session creation admits an optional initial Turn", async () => {
    const { client, requests } = transport([
      { session_id: sessionId, turn: {session_id: sessionId, turn_id: turnId, sequence: 1}, close_after_idle_ms: 604800000 },
      { session_id: sessionId, turn_id: messageId, sequence: 2 },
    ])
    const { session, turn } = await client.sessions.create({ agent: "operator", input: [{ type: "text", text: "initial" }],
      computer: client.computers.ref(holdId),
      key: "thread:1",
      idempotencyKey: "start-1", })
    expect(turn.id).toBe(turnId)
    expect(JSON.parse(String(requests[0]?.init?.body))).toEqual({
      agent: "operator",
      input: [{ type: "text", text: "initial" }],
      computer_id: holdId,
      key: "thread:1",
      idempotency_key: "start-1",
    })
    expect((await session.enqueue([])).id).toBe(messageId)
    expect(requests.map((r) => new URL(r.url).pathname)).toEqual([
      "/v1/sessions",
      `/v1/sessions/${sessionId}/enqueue`,
    ])
  })
  test("empty creation does not invent a Turn and exact send never routes to new work", async () => {
    const {client, requests} = transport([
      {session_id:sessionId, turn:null, close_after_idle_ms:604800000},
      {id:operationId, turn_id:turnId, message_id:messageId, status:"accepted"},
    ])
    const empty = await client.sessions.create({agent:"operator", idempotencyKey:"empty"})
    expect(empty.turn).toBeNull()
    expect(JSON.parse(String(requests[0]?.init?.body))).toEqual({agent:"operator",idempotency_key:"empty"})
    expect("send" in empty.session).toBe(false)
    const receipt = await empty.session.turns.ref(turnId).send([{type:"text",text:"reply"}],{idempotencyKey:"exact"})
    expect(receipt).toEqual({id:messageId,status:"accepted"})
    expect(requests[1]?.url).toEndWith(`/turns/${turnId}/messages`)
    expect(JSON.parse(String(requests[1]?.init?.body))).toEqual({message:[{type:"text",text:"reply"}],idempotency_key:"exact"})
  })
  test("events have one ordered cursor and explicit retention gaps", async () => {
    const event = {
      session_id: sessionId,
      turn_id: null,
      sequence: 8,
      created_at: createdAt,
      kind: "turn.output",
      data: [],
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
    const ref = client.sessions.ref(sessionId)
    expect(await ref.events.list({ after: 7, limit: 1000 })).toEqual({
      records: [
        {
              sessionId,
          turnId: null,
          sequence: 8,
          createdAt,
          kind: "turn.output",
          data: [],
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
      data: kind.startsWith("message.") ? { message_id: messageId, status: kind.slice(8), delivery: kind === "message.rejected" ? "uncertain" : kind.slice(8) } : kind === "turn.output" ? [{ type: "text", text: "output" }] : { reason: "route_unavailable", source_seq: 11 },
    }))
    const { client } = transport([{ records, next_after: 13, has_more: false, retained_after: 0 }])
    const page = await client.sessions.ref(sessionId).events.list({ after: 7 })
    expect(page.records.map(event => event.kind)).toEqual(kinds)
    expect(page.records.map(event => event.data)).toEqual(records.map(event => event.data))
    expect(page.records.map(event => event.turnId)).toEqual(records.map(event => event.turn_id))
    expect(page.nextAfter).toBe(13)
  })
  test("event pages retain ordinary and Computer loss interruption data together", async () => {
    const data = [
      {},
      { reason: "computer_execution_lost", computerId: holdId, leaseEpoch: 1, recoveryGap: false },
      { reason: "computer_execution_lost", computerId: holdId, leaseEpoch: 2, recoveryGap: true },
    ]
    const records = data.map((value, index) => ({ session_id: sessionId, turn_id: turnId, sequence: index + 1, created_at: createdAt, kind: "turn.interrupted", data: value }))
    const { client } = transport([{ records, next_after: 3, has_more: false, retained_after: 0 }])
    const page = await client.sessions.ref(sessionId).events.list()
    expect(page.records.map(event => event.kind)).toEqual(["turn.interrupted", "turn.interrupted", "turn.interrupted"])
    expect(page.records.map(event => event.data)).toEqual(data)
    expect(page.nextAfter).toBe(3)
    const invalid = { ...records[1]!, data: { ...data[1], recoveryGap: "false" } }
    const malformed = transport([{ records: [invalid], next_after: 2, has_more: false, retained_after: 0 }])
    await expect(malformed.client.sessions.ref(sessionId).events.list()).rejects.toThrow("interruption data")
  })
  test("event stream advances its cursor across ordinary and Computer loss stops", async () => {
    const values = [{},
      { reason: "computer_execution_lost", computerId: holdId, leaseEpoch: 1, recoveryGap: false },
      { reason: "computer_execution_lost", computerId: holdId, leaseEpoch: 2, recoveryGap: true }]
    const { client, requests } = transport(values.map((data, index) => ({
      records: [{ session_id: sessionId, turn_id: turnId, sequence: index + 1, created_at: createdAt, kind: "turn.interrupted", data }],
      next_after: index + 1, has_more: index < 2, retained_after: 0,
    })))
    const stream = client.sessions.ref(sessionId).events.stream({ after: 0, limit: 1 })
    for (const data of values) expect((await stream.next()).value?.data).toEqual(data)
    await stream.return()
    expect(requests.map(request => new URL(request.url).searchParams.get("after"))).toEqual(["0", "1", "2"])
  })
  test("Turn retrieval distinguishes expired payload from an explicit null result", async () => {
    const { input: _, ...expired } = turnState
    const { client } = transport([{ ...expired, payload_expired_at: createdAt }, { ...turnState, result: null }])
    const ref = client.sessions.ref(sessionId).turns.ref(turnId)
    expect(Object.hasOwn(await ref.get(), "result")).toBe(false)
    expect(await ref.get()).toMatchObject({
      result: null,
      terminalAt: createdAt,
    })
    expect(() => parseTurnState(turnState)).toThrow("result is required")
    expect(() => parseTurnState({ ...turnState, result: undefined })).toThrow()
  })
  test("Turn retrieval exposes immutable application failure", async () => {
    const failed = { ...turnState, status: "failed", error: { code: "application_error", message: "fixture" } }
    const { client } = transport([failed])
    expect(await client.sessions.ref(sessionId).turns.ref(turnId).get()).toMatchObject({ status: "failed", error: failed.error })
    expect(() => parseTurnState({ ...failed, error: { code: "", message: "fixture" } })).toThrow()
    expect(() => parseTurnState({ ...turnState, error: failed.error })).toThrow()
  })
  test("held closing Sessions expose independent dispatch state", async () => {
    const { client } = transport([
      {
        id: sessionId,
        agent: "worker", template: "machine",
        root_session_id: sessionId,
        requester_session_id: null,
        initial_turn: { id: turnId, status: "queued" },
        activity: { process: null },
        parent_session_id: null,
        suspension: "manual", close_after_idle_ms: null, idle_since: null,
        deployment_id: holdId,
        computer_id: holdId,
        status: "closing",
        created_at: createdAt,
        updated_at: createdAt,
        active_turn_id: null,
        holds: [
          {id:holdId,session_id:sessionId,kind:"process_failed",created_at:createdAt,can_release:true},
          {id:null,session_id:null,kind:"interrupt",created_at:createdAt,can_release:false},
        ],
      },
    ])
    expect(await client.sessions.get(sessionId)).toMatchObject({
      status: "closing",
      holds: [{ id:holdId,sessionId,kind:"process_failed",canRelease:true },{ id:null,sessionId:null,kind:"interrupt",canRelease:false }],
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
    const ref = client.sessions.ref(sessionId)
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
  const rejection = new MessageRejected("not applicable")
  expect(rejection).toBeInstanceOf(MessageRejected)
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

test("pending process stop retains its deadline", async () => {
  const deadline = createdAt
  const { client } = transport([{ ...turnState, status: "interrupted", stop: { process_epoch: 1, turn_id: turnId, state: "requested", deadline } }])
  const turn = await client.sessions.ref(sessionId).turns.ref(turnId).get()
  expect(turn.stop).toEqual({ processEpoch: 1, turnId, state: "requested", deadline })
  for (const invalid of [null, undefined]) {
    expect(() => parseTurnState({ ...turnState, result: null, stop: { process_epoch: 1, turn_id: turnId, state: "requested", deadline: invalid } })).toThrow()
  }
})
