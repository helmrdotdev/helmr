import { describe, expect, test } from "bun:test"
import {
  HelmrClient,
  MessageRejected,
  actor,
  sessions,
  workspaces,
} from "./index"
import {
  installRuntimeOperations,
  parseSessionEvent,
  parseTurnState,
  type RuntimeOperations,
} from "./internal"

const sessionId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33"
const turnId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34"
const runId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
const holdId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35"
const operationId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36"
const messageId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37"
const createdAt = "2026-07-24T11:50:00Z"
const turnState = {
  id: turnId,
  session_id: sessionId,
  sequence: 1,
  input: null,
  source: { type: "external" },
  status: "completed",
  created_at: createdAt,
  interrupt_requested: false,
  accepts_messages: false,
  terminal_event_id: operationId,
  workspace_version_id: holdId,
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
  test("Actor starts with a separate Session admission", async () => {
    const { client, requests } = transport([
      { session_id: sessionId, run_id: runId },
      { id: operationId, kind: "enqueued", turn_id: turnId },
    ])
    const { session, run } = await client.actors.start("operator", {
      workspace: workspaces.ref(holdId),
      key: "thread:1",
      idempotencyKey: "start-1",
      run: { retry: { maxAttempts: 2 } },
    })
    expect(run.id).toBe(runId)
    expect(JSON.parse(String(requests[0]?.init?.body))).toEqual({
      workspace: { id: holdId },
      key: "thread:1",
      idempotency_key: "start-1",
      run: { retry: { max_attempts: 2 } },
    })
    expect((await session.enqueue(null)).id).toBe(turnId)
    expect(requests.map((r) => new URL(r.url).pathname)).toEqual([
      "/v1/actors/operator/start",
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
    const ref = client.sessions.ref(sessionId),
      signal = new AbortController().signal
    const first = await ref.send(
      { type: "work" },
      { idempotencyKey: "send-1" },
      { signal },
    )
    expect(first.kind).toBe("enqueued")
    expect(first.turn.id).toBe(turnId)
    expect(first.turn.sessionId).toBe(sessionId)
    const second = await ref.send({ type: "reply" })
    expect(second).toMatchObject({
      kind: "messaged",
      turn: { id: turnId },
      message: { id: messageId, status: "accepted" },
    })
    expect(await first.turn.send(null)).toEqual({
      id: messageId,
      status: "accepted",
    })
    expect(requests[2]?.url).toEndWith(`/turns/${turnId}/messages`)
    expect(JSON.parse(String(requests[0]?.init?.body))).toEqual({
      data: { type: "work" },
      idempotency_key: "send-1",
    })
    expect(requests[0]?.init?.signal).toBe(signal)
    const keys = requests
      .slice(1)
      .map((r) => JSON.parse(String(r.init?.body)).idempotency_key)
    expect(keys[0]).toMatch(/^[0-9a-f-]{36}$/)
    expect(keys[1]).not.toBe(keys[0])
  })
  test("events have one ordered cursor, nullable provenance and explicit retention gaps", async () => {
    const event = {
      id: operationId,
      session_id: sessionId,
      turn_id: null,
      sequence: 8,
      created_at: createdAt,
      kind: "output",
      data: null,
      provenance: null,
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
          id: operationId,
          sessionId,
          turnId: null,
          sequence: 8,
          createdAt,
          kind: "output",
          data: null,
          provenance: null,
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
  test("Turn retrieval retains absent and explicit null results", async () => {
    const { client } = transport([turnState, { ...turnState, result: null }])
    const ref = client.sessions.ref(sessionId).turn(turnId)
    expect(Object.hasOwn(await ref.retrieve(), "result")).toBe(false)
    expect(await ref.retrieve()).toMatchObject({
      result: null,
      terminalEventId: operationId,
      workspaceVersionId: holdId,
    })
    expect(() => parseTurnState({ ...turnState, result: undefined })).toThrow()
  })
  test("held closing Sessions expose independent dispatch state", async () => {
    const { client } = transport([
      {
        id: sessionId,
        actor_id: "operator",
        deployment_id: holdId,
        workspace_id: holdId,
        status: "closing",
        created_at: createdAt,
        updated_at: createdAt,
        current_run_id: null,
        active_turn_id: null,
        dispatch: {
          state: "held",
          hold_id: holdId,
          reason: "recovery_required",
        },
      },
    ])
    expect(await client.sessions.retrieve(sessionId)).toMatchObject({
      status: "closing",
      currentRunId: null,
      activeTurnId: null,
      dispatch: { state: "held", holdId, reason: "recovery_required" },
    })
  })
  test("controls carry exact target identities", async () => {
    const receipt = {
      id: operationId,
      session_id: sessionId,
      status: "accepted",
    }
    const { client, requests } = transport([
      { ...receipt, turn_id: turnId, hold_id: holdId },
      { ...receipt, hold_id: holdId },
      receipt,
      receipt,
    ])
    const ref = client.sessions.ref(sessionId)
    expect(
      await ref.turn(turnId).interrupt({ idempotencyKey: "stop-1" }),
    ).toMatchObject({ turnId, holdId, status: "accepted" })
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
  test("Run cancellation preserves Actor acceptance instead of claiming a terminal Run", async () => {
    const { client, requests } = transport([
      {
        id: operationId,
        run_id: runId,
        session_id: sessionId,
        hold_id: holdId,
        status: "accepted",
      },
    ])
    expect(
      await client.runs.cancel(runId, { idempotencyKey: "cancel-1" }),
    ).toEqual({ id: operationId, runId, sessionId, holdId, status: "accepted" })
    expect(JSON.parse(String(requests[0]?.init?.body))).toEqual({
      idempotency_key: "cancel-1",
    })
  })
  test("event output requires serializable data and fixed lifecycle kind", () => {
    const event = {
      id: operationId,
      session_id: sessionId,
      turn_id: null,
      sequence: 1,
      created_at: createdAt,
      kind: "output",
      provenance: null,
    }
    expect(() => parseSessionEvent(event)).toThrow()
    expect(() =>
      parseSessionEvent({ ...event, data: null, kind: "application.event" }),
    ).toThrow("kind")
  })
})

test("runtime Session refs dispatch receipts and preserve caller abort scope", async () => {
  const calls: unknown[] = []
  const operations = {
    actorStart: async () => ({ sessionId, runId }),
    sessionSend: async (id, data, request, signal) => {
      calls.push({ id, data, request, signal })
      return { id: operationId, kind: "messaged", turnId, messageId }
    },
    sessionTurnSend: async (id, turn, data, request, signal) => {
      calls.push({ id, turn, data, request, signal })
      return { id: operationId, turnId: turn, messageId, status: "accepted" }
    },
    sessionEvents: async (id, query, signal) => {
      calls.push({ id, query, signal })
      return { records: [], nextAfter: 7, hasMore: false, retainedAfter: 0 }
    },
  } satisfies Partial<RuntimeOperations>
  const uninstall = installRuntimeOperations(operations as RuntimeOperations)
  try {
    const { session } = await actor({ id: "operator", run() {} }).start({
      workspace: workspaces.ref(holdId),
    })
    const signal = new AbortController().signal
    const admission = await session.send(
      null,
      { idempotencyKey: "key" },
      { signal },
    )
    expect(admission).toMatchObject({
      kind: "messaged",
      turn: { id: turnId },
      message: { id: messageId, status: "accepted" },
    })
    await admission.turn.send(
      { approved: true },
      { idempotencyKey: "reply" },
      { signal },
    )
    expect(await session.events.list({ after: 7 }, { signal })).toMatchObject({
      nextAfter: 7,
      hasMore: false,
    })
    expect(calls).toEqual([
      { id: sessionId, data: null, request: { idempotencyKey: "key" }, signal },
      {
        id: sessionId,
        turn: turnId,
        data: { approved: true },
        request: { idempotencyKey: "reply" },
        signal,
      },
      { id: sessionId, query: { after: 7 }, signal },
    ])
    expect(Object.hasOwn(sessions.ref(sessionId), "recover")).toBe(false)
  } finally {
    uninstall()
  }
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
