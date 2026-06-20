import { afterEach, expect, test } from "bun:test"

import { HelmrClient, waitpointTokenClientMethod } from "./client"
import { runStateBooleans } from "./run"
import { PayloadSchemaValidationError, idempotencyKeys, image, sandbox, source, task, type PayloadSchema } from "../index"
import { HELMR_API_VERSION, HELMR_API_VERSION_HEADER, HELMR_SDK_VERSION, HELMR_SDK_VERSION_HEADER } from "../version"

const originalFetch = globalThis.fetch
const originalEnv = { ...process.env }
const originalWarn = console.warn
const testGitSha = "0123456789abcdef0123456789abcdef01234567"

afterEach(() => {
  globalThis.fetch = originalFetch
  process.env = { ...originalEnv }
  console.warn = originalWarn
})

test("constructor requires url option or HELMR_API_URL", () => {
  delete process.env["HELMR_API_URL"]
  delete process.env["HELMR_API_KEY"]

  expect(() => new HelmrClient({})).toThrow("requires a url option or HELMR_API_URL")
})

test("constructor reads HELMR_API_URL directly without fromEnv helper", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  process.env["HELMR_API_KEY"] = "env-token"
  let authorization: string | null | undefined
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    authorization = new Headers(init?.headers).get("authorization")
    return Response.json({
      id: "run-1",
      task_id: "inspect",
      status: "succeeded",
      exit_code: 0,
    })
  }) as typeof fetch

  const client = new HelmrClient({})
  await client.runs.retrieve("run-1")

  expect(authorization).toBe("Bearer env-token")
  expect((HelmrClient as unknown as { fromEnv?: unknown }).fromEnv).toBeUndefined()
})

test("sends pinned API and SDK version headers", async () => {
  let apiVersion: string | null | undefined
  let sdkVersion: string | null | undefined
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    const headers = new Headers(init?.headers)
    apiVersion = headers.get(HELMR_API_VERSION_HEADER)
    sdkVersion = headers.get(HELMR_SDK_VERSION_HEADER)
    return Response.json({
      id: "run-1",
      task_id: "inspect",
      status: "succeeded",
      exit_code: 0,
    })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  await client.runs.retrieve("run-1")

  expect(apiVersion).toBe(HELMR_API_VERSION)
  expect(sdkVersion).toBe(HELMR_SDK_VERSION)
})

test("constructor only supports http and https URLs", () => {
  expect(() => new HelmrClient({ url: "api.example.test" })).toThrow(
    "HelmrClient requires an http(s) URL",
  )
  expect(() => new HelmrClient({ url: "unix:///tmp/helmr.sock" })).toThrow(
    "unsupported HelmrClient transport scheme unix",
  )
  expect(() => new HelmrClient({ url: "https://api.example.test?x=1", apiKey: "token" })).toThrow(
    "must not include query or fragment",
  )
})

test("client preserves configured base path when building request URLs", async () => {
  let requestedUrl: string | undefined
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrl = String(input)
    return Response.json({
      id: "run-1",
      task_id: "inspect",
      status: "succeeded",
    })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test/helmr", apiKey: "token" })
  await client.runs.retrieve("run-1")

  expect(requestedUrl).toBe("https://api.example.test/helmr/api/runs/run-1")
})

test("schedules ignore response workspace metadata", async () => {
  globalThis.fetch = (async () => {
    return Response.json({
      id: "schedule-1",
      project_id: "00000000-0000-0000-0000-000000000101",
      environment_id: "00000000-0000-0000-0000-000000000102",
      task: "inspect",
      deduplication_key: "inspect-main",
      external_id: "customer-1",
      cron: "0 * * * *",
      timezone: "UTC",
      active: true,
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const schedule = await client.schedules.retrieve("schedule-1")

  expect(schedule.task).toBe("inspect")
  expect(schedule.deduplicationKey).toBe("inspect-main")
  expect(schedule.externalId).toBe("customer-1")
  expect("workspace" in schedule).toBe(false)
})

test("schedules create uses public field names", async () => {
  let requestBody: unknown
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    requestBody = JSON.parse(String(init?.body))
    return Response.json({
      id: "schedule-1",
      type: "imperative",
      project_id: "00000000-0000-0000-0000-000000000101",
      environment_id: "00000000-0000-0000-0000-000000000102",
      task: "inspect",
      deduplication_key: "inspect-customer-1",
      external_id: "customer-1",
      cron: "0 * * * *",
      timezone: "UTC",
      active: true,
      status: "active",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  await client.schedules.create({
    task: "inspect",
    deduplicationKey: "inspect-customer-1",
    externalId: "customer-1",
    cron: "0 * * * *",
    active: false,
    options: {
      queue: "inspect",
      maxDurationSeconds: 600,
    },
  })

  expect(requestBody).toEqual({
    task: "inspect",
    deduplication_key: "inspect-customer-1",
    external_id: "customer-1",
    cron: "0 * * * *",
    active: false,
    options: {
      queue: { name: "inspect" },
      max_duration_seconds: 600,
    },
  })
})

test("schedules map next fire response fields", async () => {
  let requestBody: unknown
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    requestBody = JSON.parse(String(init?.body))
    return Response.json({
      id: "schedule-1",
      type: "imperative",
      project_id: "00000000-0000-0000-0000-000000000101",
      environment_id: "00000000-0000-0000-0000-000000000102",
      task: "inspect",
      deduplication_key: "inspect-main",
      cron: "0 * * * *",
      timezone: "UTC",
      active: true,
      status: "active",
      next_fire_at: "2026-01-01T01:00:00Z",
      last_fire_at: "2026-01-01T00:00:00Z",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const schedule = await client.schedules.create({
    deduplicationKey: "inspect-main",
    task: "inspect",
    cron: "0 * * * *",
  })

  expect(requestBody).toEqual({
    deduplication_key: "inspect-main",
    task: "inspect",
    cron: "0 * * * *",
  })
  expect(schedule.nextFireAt).toBe("2026-01-01T01:00:00Z")
  expect(schedule.lastFireAt).toBe("2026-01-01T00:00:00Z")
})

test("workspaces.open is lazy and does not call materialize or connect", () => {
  let calls = 0
  globalThis.fetch = (async () => {
    calls += 1
    return Response.json({})
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const workspace = client.workspaces.open("workspace-1")

  expect(workspace.id).toBe("workspace-1")
  expect(calls).toBe(0)
})

test("workspaces create list update materialize and connect use workspace routes", async () => {
  const requests: Array<{ url: string; method: string; body: unknown }> = []
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    const method = init?.method ?? "GET"
    const body = init?.body === undefined ? undefined : JSON.parse(String(init.body))
    requests.push({ url, method, body })
    if (url.endsWith("/materialize") || url.endsWith("/connect")) {
      return Response.json(workspaceMaterializationFixture())
    }
    if (method === "GET" && url.includes("/workspaces?")) {
      return Response.json({ workspaces: [workspaceFixture({ tags: ["prod"] })] })
    }
    return Response.json({ workspace: workspaceFixture({ metadata: { owner: "platform" } }) })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const created = await client.workspaces.create({
    projectId: "project-1",
    environmentId: "env-1",
    sandboxId: "sandbox-1",
    deploymentId: "deployment-1",
    externalId: "case-1",
    metadata: { owner: "platform" },
    tags: ["prod"],
    idempotencyKey: "workspace-key",
    idempotencyKeyTTL: "24h",
  })
  const listed = await client.workspaces.list({ projectId: "project-1", environmentId: "env-1", state: "active", tag: "prod" })
  const updated = await client.workspaces.update("workspace-1", { metadata: { owner: "platform" }, tags: ["prod"] })
  const materialized = await client.workspaces.materialize("workspace-1")
  const connected = await client.workspaces.open("workspace-1").connect()

  expect(created.metadata).toEqual({ owner: "platform" })
  expect(listed[0]?.tags).toEqual(["prod"])
  expect(updated.id).toBe("workspace-1")
  expect(materialized.workspaceId).toBe("workspace-1")
  expect(connected.workspaceId).toBe("workspace-1")
  expect(requests.map((request) => [request.method, request.url, request.body])).toEqual([
    ["POST", "https://api.example.test/api/projects/project-1/environments/env-1/workspaces", {
      deployment_id: "deployment-1",
      environment_id: "env-1",
      external_id: "case-1",
      idempotency_key: "workspace-key",
      idempotency_key_ttl: "24h",
      metadata: { owner: "platform" },
      project_id: "project-1",
      sandbox_id: "sandbox-1",
      tags: ["prod"],
    }],
    ["GET", "https://api.example.test/api/projects/project-1/environments/env-1/workspaces?project_id=project-1&environment_id=env-1&state=active&tag=prod", undefined],
    ["PATCH", "https://api.example.test/api/workspaces/workspace-1", { metadata: { owner: "platform" }, tags: ["prod"] }],
    ["POST", "https://api.example.test/api/workspaces/workspace-1/materialize", {}],
    ["POST", "https://api.example.test/api/workspaces/workspace-1/connect", {}],
  ])
})

test("http transport is explicit and warns, including localhost", async () => {
  const warnings: unknown[][] = []
  console.warn = (...args: unknown[]) => warnings.push(args)
  let requestedUrl: string | undefined
  let authorization: string | null | undefined
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrl = String(input)
    authorization = new Headers(init?.headers).get("authorization")
    return Response.json({
      id: "run-1",
      task_id: "inspect",
      status: "succeeded",
      exit_code: 0,
    })
  }) as typeof fetch

  const client = new HelmrClient({ url: "http://127.0.0.1:8080", apiKey: "dev-token" })
  await client.runs.retrieve("run-1")

  expect(requestedUrl).toBe("http://127.0.0.1:8080/api/runs/run-1")
  expect(authorization).toBe("Bearer dev-token")
  expect(warnings).toHaveLength(1)
  expect(String(warnings[0]?.[0])).toContain("http:// transport is plaintext")
})

test("http transport rejects non-loopback hosts", () => {
  expect(() => new HelmrClient({ url: "http://api.example.test", apiKey: "token" })).toThrow(
    "refusing to send credentials over plaintext non-loopback URL",
  )
  expect(() => new HelmrClient({ url: "http://192.168.1.10:8080", apiKey: "token" })).toThrow(
    "refusing to send credentials over plaintext non-loopback URL",
  )
})

test("http transport allows loopback IPv4, IPv6, and localhost", () => {
  console.warn = () => {}

  expect(() => new HelmrClient({ url: "http://127.0.0.2:8080", apiKey: "token" })).not.toThrow()
  expect(() => new HelmrClient({ url: "http://[::1]:8080", apiKey: "token" })).not.toThrow()
  expect(() => new HelmrClient({ url: "http://localhost:8080", apiKey: "token" })).not.toThrow()
})

test("tasks.start returns session and run handles from the task start response", async () => {
  let requestedUrl: string | undefined
  let body: unknown
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrl = String(input)
    body = JSON.parse(String(init?.body))
    return Response.json(taskStartFixture(
      { id: "run-1", task_id: "inspect", status: "running" },
      { isCached: true },
    ))
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const key = idempotencyKeys.create("case-123")
  const started = await client.tasks.start("inspect", { issue: 123 }, {
    projectId: "project-1",
    environmentId: "env-1",
    externalId: "case-123",
    workspaceId: "workspace-1",
    idempotencyKey: key,
    idempotencyKeyTTL: "24h",
    expiresAt: "2026-04-21T00:00:00Z",
  })

  expect(requestedUrl).toBe("https://api.example.test/api/tasks/inspect/start")
  expect(body).toEqual({
    project_id: "project-1",
    environment_id: "env-1",
    payload: { issue: 123 },
    external_id: "case-123",
    options: {
      idempotency_key: key.value,
      idempotency_key_ttl: "24h",
      expires_at: "2026-04-21T00:00:00.000Z",
      workspace_id: "workspace-1",
    },
  })
  expect(started.run).toEqual({ id: "run-1", taskId: "inspect" })
  expect(started.session.id).toBe("session-1")
  expect(started.session.currentRunId).toBe("run-1")
  expect(started.isCached).toBe(true)
})

test("tasks.startAndWait posts one start-and-wait request and returns the session", async () => {
  let requestedUrl: string | undefined
  let body: unknown
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrl = String(input)
    body = JSON.parse(String(init?.body))
    return Response.json(taskSessionFixture({
      status: "completed",
      current_run_id: null,
      result: { ok: true },
      timed_out: false,
    }))
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const session = await client.tasks.startAndWait("inspect", { timeoutSeconds: 30 })

  expect(requestedUrl).toBe("https://api.example.test/api/tasks/inspect/start-and-wait")
  expect(body).toEqual({ timeout_seconds: 30 })
  expect(session.status).toBe("completed")
  expect(session.result).toEqual({ ok: true })
})

test("tasks.startAndWait retries a pending idempotent start before returning the session", async () => {
  const requestedUrls: string[] = []
  let calls = 0
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrls.push(String(input))
    calls++
    if (calls === 1) {
      return Response.json({ code: "idempotency_pending", retry_after_ms: 1 }, {
        status: 202,
        headers: { "retry-after": "0" },
      })
    }
    return Response.json(taskSessionFixture({
      status: "completed",
      current_run_id: null,
      result: { ok: true },
    }))
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const session = await client.tasks.startAndWait("inspect", { idempotencyKey: "retry-key" })

  expect(requestedUrls).toEqual([
    "https://api.example.test/api/tasks/inspect/start-and-wait",
    "https://api.example.test/api/tasks/inspect/start-and-wait",
  ])
  expect(session.status).toBe("completed")
  expect(session.result).toEqual({ ok: true })
})

test("sessions facade retrieves state and reads/writes session channels", async () => {
  const requestedUrls: string[] = []
  const bodies: unknown[] = []
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    requestedUrls.push(url)
    if (init?.body !== undefined) {
      bodies.push(JSON.parse(String(init.body)))
    }
    if (url.endsWith("/api/sessions/session-1")) {
      return Response.json(taskSessionFixture({ id: "session-1", external_id: "case-123" }))
    }
    if (url.endsWith("/api/sessions/session-1/wait")) {
      return Response.json(taskSessionFixture({ id: "session-1", status: "completed", result: { ok: true } }))
    }
    if (url.endsWith("/api/sessions/session-1/channels/approval/inputs")) {
      return Response.json({
        record: channelRecordFixture({ data: { approved: true }, correlation_id: "thread-1" }),
        idempotency_status: "created",
      }, { status: 201 })
    }
    if (url.includes("/api/sessions/session-1/channels/agent.report/outputs")) {
      return Response.json({
        records: [
          channelRecordFixture({ sequence: 2, data: { text: "ready" }, content_type: "application/json" }),
        ],
      })
    }
    throw new Error(`unexpected URL ${url}`)
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const session = client.sessions.open("session-1")
  expect((await session.retrieve()).externalId).toBe("case-123")
  expect((await session.wait({ timeoutSeconds: 10 })).result).toEqual({ ok: true })
  expect(await session.input("approval").send({ approved: true }, { correlationId: "thread-1" })).toMatchObject({
    data: { approved: true },
    correlationId: "thread-1",
    idempotencyStatus: "created",
  })
  expect(await session.output("agent.report").list({ cursor: 1, limit: 10, correlationId: "thread-1" })).toEqual([
    {
      id: "record-1",
      channelId: "channel-1",
      sequence: 2,
      data: { text: "ready" },
      contentType: "application/json",
      createdAt: "2026-04-20T00:00:00Z",
    },
  ])
  expect(requestedUrls).toEqual([
    "https://api.example.test/api/sessions/session-1",
    "https://api.example.test/api/sessions/session-1/wait",
    "https://api.example.test/api/sessions/session-1/channels/approval/inputs",
    "https://api.example.test/api/sessions/session-1/channels/agent.report/outputs?after_sequence=1&limit=10&correlation_id=thread-1",
  ])
  expect(bodies).toEqual([
    { timeout_seconds: 10 },
    { data: { approved: true }, correlation_id: "thread-1" },
  ])
})

test("session output stream maps channel record SSE frames", async () => {
  const encoder = new TextEncoder()
  const requestedUrls: string[] = []
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = String(input)
    requestedUrls.push(url)
    if (url.endsWith("/api/sessions/session-1")) {
      return Response.json(taskSessionFixture({ status: "completed", current_run_id: null }))
    }
    const record = channelRecordFixture({ sequence: 7, data: { text: "done" } })
    return new Response(
      new ReadableStream({
        start(controller) {
          controller.enqueue(encoder.encode(`id: 7\nevent: channel_output\ndata: ${JSON.stringify(record)}\n\n`))
          controller.close()
        },
      }),
      { status: 200, headers: { "content-type": "text/event-stream" } },
    )
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const records: unknown[] = []
  for await (const record of await client.sessions.open("session-1").output("agent.report").stream({ cursor: 6, correlationId: "thread-1" })) {
    records.push(record)
  }

  expect(requestedUrls).toEqual([
    "https://api.example.test/api/sessions/session-1/channels/agent.report/outputs/stream?after_sequence=6&correlation_id=thread-1",
    "https://api.example.test/api/sessions/session-1",
  ])
  expect(records).toEqual([{
    id: "record-1",
    channelId: "channel-1",
    sequence: 7,
    data: { text: "done" },
    contentType: "application/json",
    createdAt: "2026-04-20T00:00:00Z",
  }])
})

test("session output stream reconnects with the last yielded sequence", async () => {
  const encoder = new TextEncoder()
  const requestedUrls: string[] = []
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = String(input)
    requestedUrls.push(url)
    if (url.endsWith("/api/sessions/session-1")) {
      const openChecks = requestedUrls.filter((requested) => requested.endsWith("/api/sessions/session-1")).length
      return Response.json(taskSessionFixture({
        status: openChecks === 1 ? "open" : "completed",
        current_run_id: openChecks === 1 ? "run-1" : null,
      }))
    }
    const sequence = url.includes("after_sequence=7") ? 8 : 7
    const record = channelRecordFixture({ id: `record-${sequence}`, sequence, data: { text: `step-${sequence}` } })
    return new Response(
      new ReadableStream({
        start(controller) {
          controller.enqueue(encoder.encode(`id: ${sequence}\nevent: channel_output\ndata: ${JSON.stringify(record)}\n\n`))
          controller.close()
        },
      }),
      { status: 200, headers: { "content-type": "text/event-stream" } },
    )
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const records: unknown[] = []
  for await (const record of await client.sessions.open("session-1").output("agent.report").stream({ cursor: 6 })) {
    records.push(record)
  }

  expect(requestedUrls).toEqual([
    "https://api.example.test/api/sessions/session-1/channels/agent.report/outputs/stream?after_sequence=6",
    "https://api.example.test/api/sessions/session-1",
    "https://api.example.test/api/sessions/session-1/channels/agent.report/outputs/stream?after_sequence=7",
    "https://api.example.test/api/sessions/session-1",
  ])
  expect(records).toMatchObject([
    { sequence: 7, data: { text: "step-7" } },
    { sequence: 8, data: { text: "step-8" } },
  ])
})

test("auth.createPublicToken posts a scoped public access token request", async () => {
  let requestedUrl: string | undefined
  let body: unknown
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrl = String(input)
    body = JSON.parse(String(init?.body))
    return Response.json({
      id: "public-token-id",
      public_access_token: "hlmr_pat_secret",
      scope: {
        type: "session.output.read",
        session_id: "session-1",
        channel: "agent.report",
        correlation_id: "thread-1",
      },
      expires_at: "2026-04-20T01:00:00Z",
      max_uses: 5,
      created_at: "2026-04-20T00:00:00Z",
    }, { status: 201 })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const token = await client.auth.createPublicToken({
    scope: {
      type: "session.output.read",
      sessionId: "session-1",
      channel: "agent.report",
      correlationId: "thread-1",
    },
    expiresAt: new Date("2026-04-20T01:00:00Z"),
    maxUses: 5,
  })

  expect(requestedUrl).toBe("https://api.example.test/api/public-access-tokens")
  expect(body).toEqual({
    scope: {
      type: "session.output.read",
      session_id: "session-1",
      channel: "agent.report",
      correlation_id: "thread-1",
    },
    expires_at: "2026-04-20T01:00:00.000Z",
    max_uses: 5,
  })
  expect(token).toEqual({
    id: "public-token-id",
    publicAccessToken: "hlmr_pat_secret",
    scope: {
      type: "session.output.read",
      sessionId: "session-1",
      channel: "agent.report",
      correlationId: "thread-1",
    },
    expiresAt: "2026-04-20T01:00:00Z",
    maxUses: 5,
    createdAt: "2026-04-20T00:00:00Z",
  })
})

test("waitpoints token namespace exposes create and complete", async () => {
  const requestedUrls: string[] = []
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrls.push(String(input))
    if (init?.method === "POST" && String(input).endsWith("/api/waitpoints/tokens")) {
      return Response.json({
        id: "token-id",
        callback_url: "https://api.example.test/api/waitpoints/tokens/token-id/callback/callback-secret",
        public_access_token: "public-token",
        timeout_at: null,
      }, { status: 201 })
    }
    return new Response(null, { status: 204 })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const token = await client.waitpoints.tokens.create()
  await client.waitpoints.tokens.complete(token, { approved: true })

  expect(requestedUrls).toEqual([
    "https://api.example.test/api/waitpoints/tokens",
    "https://api.example.test/api/waitpoints/tokens/token-id/complete",
  ])
})

test("task.start returns the task start response with session and run handles", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  process.env["HELMR_API_KEY"] = "token"
  let requestedUrl: string | undefined
  let body: unknown
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrl = String(input)
    body = JSON.parse(String(init?.body))
    return Response.json(
      taskStartFixture({
        id: "018f0000000070008000000000000001",
        task_id: "inspect",
        status: "running",
      }),
      {
        status: 201,
      },
    )
  }) as typeof fetch

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    run: async () => undefined,
  })
  const started = await inspect.start({
  })

  expect(requestedUrl).toBe("https://api.example.test/api/tasks/inspect/start")
  expect(body).toEqual({
    options: { max_duration_seconds: 900 },
  })
  expect(started).toMatchObject({
    session: { id: "session-1", taskId: "inspect", currentRunId: "018f0000000070008000000000000001" },
    run: { id: "018f0000000070008000000000000001", taskId: "inspect" },
    isCached: false,
  })
})

test("task.start posts idempotency options", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  let body: unknown
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    body = JSON.parse(String(init?.body))
    return Response.json(taskStartFixture({ id: "run-1", task_id: "inspect", status: "queued" }), { status: 201 })
  }) as typeof fetch

  const key = idempotencyKeys.create(["deploy", "prod"], { scope: "global" })
  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    run: async () => undefined,
  })
  await inspect.start({
    idempotencyKey: key,
    idempotencyKeyTTL: "24h",
  })

  expect(body).toMatchObject({
    options: {
      idempotency_key: key.value,
      idempotency_key_ttl: "24h",
      max_duration_seconds: 900,
    },
  })
})

test("task.start retries pending task start before returning the start response", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  let calls = 0
  let pendingBodyPulls = 0
  globalThis.fetch = (async () => {
    calls += 1
    if (calls === 1) {
      return new Response(
        new ReadableStream({
          pull(controller) {
            pendingBodyPulls += 1
            controller.enqueue(new TextEncoder().encode(`{"code":"idempotency_pending","error":"task_start_pending"}`))
            controller.close()
          },
        }),
        { status: 202, headers: { "retry-after": "0.001" } },
      )
    }
    return Response.json(taskStartFixture({ id: "run-1", task_id: "inspect", status: "queued" }), { status: 200 })
  }) as typeof fetch

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    run: async () => undefined,
  })

  await expect(inspect.start({ idempotencyKey: idempotencyKeys.create("inspect") })).resolves.toMatchObject({
    session: { id: "session-1", taskId: "inspect", currentRunId: "run-1" },
    run: { id: "run-1", taskId: "inspect" },
  })
  expect(calls).toBe(2)
  expect(pendingBodyPulls).toBe(1)
})

test("task.start does not retry non-pending accepted responses", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  let calls = 0
  globalThis.fetch = (async () => {
    calls += 1
    return Response.json({ error: "accepted elsewhere" }, { status: 202 })
  }) as typeof fetch

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    run: async () => undefined,
  })

  await expect(inspect.start({ idempotencyKey: idempotencyKeys.create("inspect") })).rejects.toThrow(
    "accepted elsewhere",
  )
  expect(calls).toBe(1)
})

test("task.start backs off for stale HTTP-date retry-after headers", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  let calls = 0
  globalThis.fetch = (async () => {
    calls += 1
    if (calls === 1) {
      return Response.json(
        { code: "idempotency_pending", error: "task_start_pending" },
        { status: 202, headers: { "retry-after": new Date(Date.now() - 1000).toUTCString() } },
      )
    }
    return Response.json(taskStartFixture({ id: "run-1", task_id: "inspect", status: "queued" }), { status: 200 })
  }) as typeof fetch

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    run: async () => undefined,
  })

  const startedAt = Date.now()
  await inspect.start({ idempotencyKey: idempotencyKeys.create("inspect") })

  expect(calls).toBe(2)
  expect(Date.now() - startedAt).toBeGreaterThanOrEqual(100)
})

test("task.start posts scheduling options", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  let body: unknown
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    body = JSON.parse(String(init?.body))
    return Response.json(taskStartFixture({ id: "run-1", task_id: "inspect", status: "queued" }), { status: 201 })
  }) as typeof fetch

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    run: async () => undefined,
  })
  await inspect.start({
    queue: "review/pr",
    concurrencyKey: "repo:42",
    priority: 30,
    ttl: "10m",
  })

  expect(body).toMatchObject({
    options: {
      queue: { name: "review/pr" },
      concurrency_key: "repo:42",
      priority: 30,
      ttl: "10m",
      max_duration_seconds: 900,
    },
  })
})

test("task.start posts retry metadata and tags", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  let body: unknown
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    body = JSON.parse(String(init?.body))
    return Response.json(taskStartFixture({ id: "run-1", task_id: "inspect", status: "queued" }), { status: 201 })
  }) as typeof fetch

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    run: async () => undefined,
  })
  await inspect.start({
    retry: { maxAttempts: 3, backoff: { minMs: 1000, maxMs: 30000, factor: 2, jitter: "full" } },
    metadata: { customer: "acme" },
    tags: ["nightly", "prod"],
  })

  expect(body).toMatchObject({
    options: {
      retry: { maxAttempts: 3, backoff: { minMs: 1000, maxMs: 30000, factor: 2, jitter: "full" } },
      metadata: { customer: "acme" },
      tags: ["nightly", "prod"],
      max_duration_seconds: 900,
    },
  })
})

test("task.start rejects unsupported retry fields before posting", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  let posted = false
  globalThis.fetch = (async () => {
    posted = true
    return Response.json({ id: "run-1", task_id: "inspect", status: "queued" }, { status: 201 })
  }) as typeof fetch

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    run: async () => undefined,
  })

  await expect(inspect.start({
    retry: { maxAttempts: 2, retryOn: ["timeout"] } as never,
  })).rejects.toThrow("retry.retryOn is not supported")
  expect(posted).toBe(false)
})

test("runs.cancel posts graceful cancel intent", async () => {
  let requestedUrl: string | undefined
  let body: unknown
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrl = String(input)
    body = JSON.parse(String(init?.body))
    return Response.json({
      run: {
        id: "run-1",
        task_id: "inspect",
        status: "cancelled",
      },
      operation: {
        id: "operation-1",
        run_id: "run-1",
        kind: "cancel",
        status: "applied",
        created_at: "2026-01-01T00:00:00Z",
      },
    })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const run = await client.runs.cancel("run-1", {
    reason: "duplicate",
    idempotencyKey: "cancel-run-1",
  })

  expect(requestedUrl).toBe("https://api.example.test/api/runs/run-1/cancel")
  expect(body).toEqual({
    reason: "duplicate",
    idempotency_key: "cancel-run-1",
  })
  expect(run.status).toBe("cancelled")
})

test("task.start validates payload before posting the start request", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  process.env["HELMR_API_KEY"] = "token"
  let fetched = false
  globalThis.fetch = (async () => {
    fetched = true
    return Response.json(taskStartFixture({ id: "run-1", task_id: "inspect", status: "running" }), { status: 201 })
  }) as typeof fetch
  const payload: PayloadSchema<{ readonly issue: string }, { readonly issue: number }> = {
    "~standard": {
      version: 1,
      vendor: "test",
      validate() {
        return { issues: [{ message: "expected string", path: ["issue"] }] }
      },
    },
  }

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    payload,
    run: async (payload) => payload.issue,
  })
  await expect(inspect.start(
    { issue: "123" },
    {},
  )).rejects.toThrow(PayloadSchemaValidationError)
  expect(fetched).toBe(false)
})

test("task.start rejects undefined payload for schema-backed tasks before posting", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  process.env["HELMR_API_KEY"] = "token"
  let fetched = false
  globalThis.fetch = (async () => {
    fetched = true
    return Response.json(taskStartFixture({ id: "run-1", task_id: "inspect", status: "running" }), { status: 201 })
  }) as typeof fetch
  const payload: PayloadSchema<undefined | { readonly issue: number }, { readonly issue: number }> = {
    "~standard": {
      version: 1,
      vendor: "test",
      validate(value) {
        return { value: value === undefined ? { issue: 0 } : value }
      },
    },
  }

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    payload,
    run: async (payload) => payload.issue,
  })
  await expect((inspect.start as (...args: any[]) => Promise<unknown>)(
    undefined,
    {},
  )).rejects.toThrow('task "inspect" requires payload')
  expect(fetched).toBe(false)
})

test("task.start rejects payload on no-payload tasks before posting", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  process.env["HELMR_API_KEY"] = "token"
  let fetched = false
  globalThis.fetch = (async () => {
    fetched = true
    return Response.json(taskStartFixture({ id: "run-1", task_id: "inspect", status: "running" }), { status: 201 })
  }) as typeof fetch

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    run: async () => undefined,
  })
  await expect((inspect.start as (...args: any[]) => Promise<unknown>)(
    undefined,
    {},
  )).rejects.toThrow('task "inspect" does not accept payload')
  expect(fetched).toBe(false)
})

test("task.start posts payload for schema-backed tasks", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  process.env["HELMR_API_KEY"] = "token"
  let body: unknown
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    body = JSON.parse(String(init?.body))
    return Response.json(taskStartFixture({ id: "run-1", task_id: "inspect", status: "running" }), { status: 201 })
  }) as typeof fetch
  const payload: PayloadSchema<{ readonly issue: number }, { readonly issue: number }> = {
    "~standard": {
      version: 1,
      vendor: "test",
      validate(value) {
        return { value: value as { readonly issue: number } }
      },
    },
  }

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    payload,
    run: async (payload) => payload.issue,
  })
  await inspect.start(
    { issue: 123 },
    {},
  )

  expect(body).toMatchObject({ payload: { issue: 123 } })
})

test("task.start validates payload and posts through the default client", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  process.env["HELMR_API_KEY"] = "token"
  let requestedUrl: string | undefined
  let authorization: string | null | undefined
  let body: unknown
  const payload: PayloadSchema<{ readonly issue: string }, { readonly issue: number }> = {
    "~standard": {
      version: 1,
      vendor: "test",
      validate(value) {
        const issue = (value as { readonly issue: string }).issue
        return { value: { issue: Number(issue) } }
      },
    },
  }
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrl = String(input)
    authorization = new Headers(init?.headers).get("authorization")
    body = JSON.parse(String(init?.body))
    return Response.json(taskStartFixture({ id: "run-1", task_id: "inspect", status: "running" }), { status: 201 })
  }) as typeof fetch

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    payload,
    run: async (payload) => payload.issue,
  })
  const started = await inspect.start(
    { issue: "123" },
    {},
  )

  expect(requestedUrl).toBe("https://api.example.test/api/tasks/inspect/start")
  expect(authorization).toBe("Bearer token")
  expect(body).toMatchObject({
    payload: { issue: "123" },
    options: { max_duration_seconds: 900 },
  })
  expect(started).toMatchObject({
    session: { id: "session-1", taskId: "inspect", currentRunId: "run-1" },
    run: { id: "run-1", taskId: "inspect" },
  })
})

test("client.tasks.start posts id-based payload without local schema validation", async () => {
  let validated = false
  let body: unknown
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    body = JSON.parse(String(init?.body))
    return Response.json(taskStartFixture({ id: "run-1", task_id: "inspect", status: "running" }), { status: 201 })
  }) as typeof fetch
  const payload: PayloadSchema<{ readonly issue: string }, { readonly issue: number }> = {
    "~standard": {
      version: 1,
      vendor: "test",
      validate() {
        validated = true
        return { issues: [{ message: "should not validate id-based starts" }] }
      },
    },
  }
  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    payload,
    run: async (payload) => payload.issue,
  })

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  await client.tasks.start<typeof inspect>(
    "inspect",
    { issue: "123" },
    {},
  )

  expect(validated).toBe(false)
  expect(body).toEqual({
    payload: { issue: "123" },
  })
})

test("runs.list reads the list response envelope", async () => {
  let requestedUrl: string | undefined
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrl = String(input)
    return Response.json({
      runs: [
        {
          id: "run-1",
          task_id: "inspect",
          status: "queued",
          exit_code: null,
          created_at: "2026-05-09T00:00:00Z",
          updated_at: "2026-05-09T00:01:00Z",
        },
      ],
    })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const runs = await client.runs.list()

  expect(requestedUrl).toBe("https://api.example.test/api/runs")
  expect(runs).toHaveLength(1)
  expect(runs[0]?.id).toBe("run-1")
  expect(runs[0]?.createdAt).toBe("2026-05-09T00:00:00Z")
  expect(runs[0]?.updatedAt).toBe("2026-05-09T00:01:00Z")
})

test("runs.retrieve accepts a run handle and returns a run snapshot", async () => {
  let requestedUrl: string | undefined
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrl = String(input)
    return Response.json({
      id: "run-1",
      task_id: "inspect",
      status: "succeeded",
      exit_code: 0,
      created_at: "2026-05-09T00:00:00Z",
      updated_at: "2026-05-09T00:01:00Z",
      pending_waitpoint: null,
    })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const run = await client.runs.retrieve({ id: "run-1", taskId: "inspect" })

  expect(requestedUrl).toBe("https://api.example.test/api/runs/run-1")
  expect(run).toEqual({
      id: "run-1",
      taskId: "inspect",
      status: "succeeded",
      attemptNumber: null,
      metadata: {},
      exitCode: 0,
    createdAt: "2026-05-09T00:00:00Z",
    updatedAt: "2026-05-09T00:01:00Z",
    pendingWaitpoint: null,
    isQueued: false,
    isRunning: false,
    isWaiting: false,
    isTerminal: true,
    isSuccess: true,
    isFailed: false,
    isCancelled: false,
  })
})

test("run state booleans only treat public running status as running", () => {
  expect(runStateBooleans("queued").isRunning).toBe(false)
  expect(runStateBooleans("running").isRunning).toBe(true)
  expect(runStateBooleans("waiting").isRunning).toBe(false)
})

test("run snapshots reject unsupported internal statuses", async () => {
  globalThis.fetch = (async (_input: RequestInfo | URL) => {
    return Response.json({
      id: "run-1",
      task_id: "inspect",
      status: "leased",
    })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })

  await expect(client.runs.retrieve("run-1")).rejects.toThrow('unsupported run status "leased"')
})

test("task.start posts a task start without workspace preparation", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  process.env["HELMR_API_KEY"] = "token"
  let body: unknown
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    body = JSON.parse(String(init?.body))
    return Response.json(
      taskStartFixture({
        id: "018f0000000070008000000000000003",
        task_id: "inspect",
        status: "running",
      }),
      { status: 201 },
    )
  }) as typeof fetch

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect")
      .image(image("inspect").from("debian:trixie-slim").copy("/app", source.directory(".")))
      .workspace("/workspace"),
    run: async () => undefined,
  })
  await inspect.start({})

  expect(body).toMatchObject({
    options: { max_duration_seconds: 900 },
  })
})

test("tasks.start leaves build validation to the remote worker", async () => {
  let requestedUrl: string | undefined
  globalThis.fetch = (async (_input: RequestInfo | URL, _init?: RequestInit) => {
    requestedUrl = String(_input)
    return Response.json(taskStartFixture({ id: "run-1", task_id: "inspect", status: "queued" }), { status: 201 })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(
      image("inspect").from("debian:trixie-slim").copy("/tool.sh", source.file("tool.sh")),
    ),
    run: async () => undefined,
  })
  await client.tasks.start<typeof inspect>("inspect", {
  })
  expect(requestedUrl).toBe("https://api.example.test/api/tasks/inspect/start")
})

test("runs.events.list maps backend audit payload fields", async () => {
  let requestedUrl: string | undefined
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrl = String(input)
    return Response.json({
      cursor: 0,
      next_cursor: null,
      events: [
        {
          id: "1",
          run_id: "run-1",
          kind: "waitpoint",
          message: "waitpoint.created",
          at: "2026-04-28T00:00:00Z",
            attributes: {
              kind: "token",
              waitpoint_id: "token-1",
              timeout: 30,
            params: { message: "Approve deploy?", timeout: 30 },
            metadata: { bridge: "slack" },
            tags: ["approval"],
          },
        },
        {
          id: "2",
          run_id: "run-1",
          kind: "waitpoint",
          message: "waitpoint.created",
          at: "2026-04-28T00:00:01Z",
            attributes: {
              kind: "token",
              waitpoint_id: "token-2",
              timeout: 60,
            params: { prompt: "What changed?", timeout: 60 },
          },
        },
        {
          id: "3",
          run_id: "run-1",
          kind: "waitpoint",
          message: "waitpoint.completed",
          at: "2026-04-28T00:00:02Z",
          attributes: {
            kind: "token",
            waitpoint_id: "token-2",
            principal: "user",
            payload: { text: "Dependency update", attachments: [] },
          },
        },
        {
          id: "4",
          run_id: "run-1",
          kind: "log",
          message: "log.stdout",
          at: "2026-04-28T00:00:03Z",
          attributes: {
            stream: "stdout",
            bytes: 128,
            observed_seq: 7,
          },
        },
        {
          id: "5",
          run_id: "run-1",
          kind: "run",
          message: "run.completed",
          at: "2026-04-28T00:00:04Z",
          attributes: {
            exit_code: 0,
          },
        },
        {
          id: "6",
          run_id: "run-1",
          kind: "run",
          message: "run.failed",
          at: "2026-04-28T00:00:05Z",
          attributes: {
            failure_kind: "max_duration",
            detail: { message: "maximum run duration exceeded", limit_seconds: 30 },
          },
        },
        {
          id: "7",
          run_id: "run-1",
          kind: "run",
          message: "run.expired",
          at: "2026-04-28T00:00:06Z",
          attributes: {
            ttl: "10m",
            message: "run ttl expired before execution started",
          },
        },
      ],
    })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const events = await client.runs.events.list("run-1")

  expect(requestedUrl).toBe("https://api.example.test/api/runs/run-1/events")
  expect(events).toEqual([
    {
      type: "waitpoint",
      run_id: "run-1",
      waitpoint_id: "token-1",
      kind: "token",
      params: { message: "Approve deploy?", timeout: 30 },
      metadata: { bridge: "slack" },
      tags: ["approval"],
      timeout: 30,
      at: "2026-04-28T00:00:00Z",
    },
    {
      type: "waitpoint",
      run_id: "run-1",
      waitpoint_id: "token-2",
      kind: "token",
      params: { prompt: "What changed?", timeout: 60 },
      metadata: {},
      tags: [],
      timeout: 60,
      at: "2026-04-28T00:00:01Z",
    },
    {
      type: "waitpoint_completed",
      run_id: "run-1",
      waitpoint_id: "token-2",
      kind: "token",
      payload: { text: "Dependency update", attachments: [] },
      at: "2026-04-28T00:00:02Z",
    },
    {
      type: "log",
      run_id: "run-1",
      stream: "stdout",
      bytes: 128,
      observed_seq: 7,
      at: "2026-04-28T00:00:03Z",
    },
    {
      type: "task_result",
      run_id: "run-1",
      exit_code: 0,
      at: "2026-04-28T00:00:04Z",
    },
    {
      type: "run_failed",
      run_id: "run-1",
      failure_kind: "max_duration",
      detail: { message: "maximum run duration exceeded", limit_seconds: 30 },
      at: "2026-04-28T00:00:05Z",
    },
    {
      type: "run_expired",
      run_id: "run-1",
      ttl: "10m",
      message: "run ttl expired before execution started",
      at: "2026-04-28T00:00:06Z",
    },
  ])
})

test("runs.events.list maps pageSize to backend limit while following pages", async () => {
  const requestedUrls: string[] = []
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrls.push(String(input))
    if (requestedUrls.length === 1) {
      return Response.json({ cursor: 0, next_cursor: 25, events: [] })
    }
    return Response.json({ cursor: 25, next_cursor: null, events: [] })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const events = await client.runs.events.list("run-1", { cursor: 10, pageSize: 25 })

  expect(events).toEqual([])
  expect(requestedUrls).toEqual([
    "https://api.example.test/api/runs/run-1/events?cursor=10&limit=25",
    "https://api.example.test/api/runs/run-1/events?cursor=25&limit=25",
  ])
})

test("runs.logs.retrieve decodes log snapshots", async () => {
  let requestedUrl: string | undefined
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrl = String(input)
    return Response.json({
      stdout_base64: "aGVsbG8=",
      stderr_base64: "d2Fybg==",
      cursor: "3:2",
      truncated: false,
    })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const logs = await client.runs.logs.retrieve("run-1")

  expect(requestedUrl).toBe("https://api.example.test/api/runs/run-1/logs")
  expect(logs).toEqual({ stdout: "hello", stderr: "warn", cursor: "3:2", truncated: false })
})

test("task.start posts the start request directly without uploading source tar", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  process.env["HELMR_API_KEY"] = "token"
  const urls: string[] = []
  let createRunBody: unknown
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    urls.push(url)
    createRunBody = JSON.parse(String(init?.body))
    return Response.json(
      taskStartFixture({
        id: "018f0000000070008000000000000002",
        task_id: "inspect",
        status: "running",
      }),
      { status: 201 },
    )
  }) as typeof fetch

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    run: async () => undefined,
  })
  await inspect.start({})

  expect(urls).toEqual(["https://api.example.test/api/tasks/inspect/start"])
  expect(createRunBody).toMatchObject({
    options: { max_duration_seconds: 900 },
  })
})

test("retrieved run snapshots expose pending waitpoints", async () => {
  globalThis.fetch = (async () => {
    return Response.json({
      id: "run-1",
      task_id: "inspect",
      status: "waiting",
      exit_code: null,
      pending_waitpoint: {
        id: "request-1",
        kind: "token",
        status: "pending",
        params: { channel: "deploy-review" },
        metadata: {},
        tags: [],
        created_at: "2026-04-20T00:00:00Z",
      },
    })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const run = await client.runs.retrieve("run-1")

  expect(run.pendingWaitpoint).toMatchObject({
    id: "request-1",
    runId: "run-1",
    kind: "token",
    status: "pending",
    params: { channel: "deploy-review" },
  })
})

test("waitpoint token method posts to the token create route", async () => {
  let requestedUrl: string | undefined
  let body: unknown
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrl = String(input)
    body = JSON.parse(String(init?.body))
    return Response.json(
      {
        id: "token-id",
        callback_url: "https://api.example.test/api/waitpoints/tokens/token-id/callback/callback-secret",
        public_access_token: "secret-token",
        timeout_at: "2026-04-20T01:00:00Z",
        status: "waiting",
        tags: ["prod"],
        metadata: { recipient: "reviewer@example.com" },
      },
      { status: 201 },
    )
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const token = await client[waitpointTokenClientMethod]({
    operation: "create",
    opts: {
      timeoutInSeconds: 3600,
      tags: ["prod"],
      metadata: { recipient: "reviewer@example.com" },
    },
  })

  expect(requestedUrl).toBe("https://api.example.test/api/waitpoints/tokens")
  expect(body).toEqual({
    timeout_in_seconds: 3600,
    tags: ["prod"],
    metadata: { recipient: "reviewer@example.com" },
  })
  expect(token).toEqual({
    id: "token-id",
    status: "waiting",
    callbackUrl: "https://api.example.test/api/waitpoints/tokens/token-id/callback/callback-secret",
    publicAccessToken: "secret-token",
    timeoutAt: "2026-04-20T01:00:00Z",
    tags: ["prod"],
    metadata: { recipient: "reviewer@example.com" },
  })
})

test("waitpoint token method can target an explicit environment", async () => {
  let requestedUrl: string | undefined
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrl = String(input)
    return Response.json({
      id: "token-id",
      callback_url: "https://api.example.test/api/waitpoints/tokens/token-id/callback/callback-secret",
      public_access_token: "secret-token",
      timeout_at: null,
    }, { status: 201 })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  await client[waitpointTokenClientMethod]({
    operation: "create",
    opts: { projectId: "project-1", environmentId: "env-1" },
  })

  expect(requestedUrl).toBe("https://api.example.test/api/projects/project-1/environments/env-1/waitpoints/tokens")
})

test("waitpoint token method retrieve maps completed data", async () => {
  let requestedUrl: string | undefined
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrl = String(input)
    return Response.json({
      id: "token-id",
      callback_url: "https://api.example.test/api/waitpoints/tokens/token-id/callback/callback-secret",
      timeout_at: "2026-04-20T01:00:00Z",
      status: "completed",
      data: { approved: true },
      metadata: { channel: "slack" },
    })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const token = await client[waitpointTokenClientMethod]({ operation: "retrieve", id: "token-id" })

  expect(requestedUrl).toBe("https://api.example.test/api/waitpoints/tokens/token-id")
  expect(token).toEqual({
    id: "token-id",
    status: "completed",
    callbackUrl: "https://api.example.test/api/waitpoints/tokens/token-id/callback/callback-secret",
    timeoutAt: "2026-04-20T01:00:00Z",
    data: { approved: true },
    metadata: { channel: "slack" },
  })
})

test("waitpoint token method complete uses token public access token when present", async () => {
  let requestedUrl: string | undefined
  let body: unknown
  let authorization: string | null | undefined
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrl = String(input)
    authorization = new Headers(init?.headers).get("authorization")
    body = JSON.parse(String(init?.body))
    return new Response(null, { status: 204 })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  await client[waitpointTokenClientMethod]({
    operation: "complete",
    token: {
      id: "token-id",
      callbackUrl: "https://api.example.test/api/waitpoints/tokens/token-id/callback/callback-secret",
      publicAccessToken: "raw-token",
      timeoutAt: null,
    },
    data: { text: "continue" },
  })

  expect(requestedUrl).toBe("https://api.example.test/api/waitpoints/tokens/token-id/complete")
  expect(authorization).toBe("Bearer raw-token")
  expect(body).toEqual({
    data: { text: "continue" },
  })
})

test("waitpoint token method complete accepts explicit public access token option", async () => {
  let authorization: string | null | undefined
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    authorization = new Headers(init?.headers).get("authorization")
    return new Response(null, { status: 204 })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  await client[waitpointTokenClientMethod]({
    operation: "complete",
    token: "token-id",
    data: { reviewed: true },
    opts: { publicAccessToken: "raw-token" },
  })

  expect(authorization).toBe("Bearer raw-token")
})

test("waitpoint token method complete accepts explicit id", async () => {
  let requestedUrl: string | undefined
  let body: unknown
  let authorization: string | null | undefined
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrl = String(input)
    authorization = new Headers(init?.headers).get("authorization")
    body = JSON.parse(String(init?.body))
    return new Response(null, { status: 204 })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  await client[waitpointTokenClientMethod]({
    operation: "complete",
    token: "token-id",
    data: { reviewed: true },
  })

  expect(requestedUrl).toBe("https://api.example.test/api/waitpoints/tokens/token-id/complete")
  expect(authorization).toBe("Bearer token")
  expect(body).toEqual({
    data: { reviewed: true },
  })
})

test("runs.events.subscribe handles CRLF SSE frames split across chunks", async () => {
  const encoder = new TextEncoder()
  const event = {
    id: "1",
    kind: "audit",
    message: "waitpoint.created",
    at: "2026-04-20T00:00:00Z",
      attributes: {
        run_id: "run-1",
        kind: "token",
        waitpoint_id: "token-1",
        params: { subject: "deploy" },
    },
  }
  globalThis.fetch = (async () =>
    new Response(
      new ReadableStream({
        start(controller) {
          controller.enqueue(encoder.encode(`data: ${JSON.stringify(event)}\r\n`))
          controller.enqueue(encoder.encode("\r\n"))
          controller.close()
        },
      }),
      { status: 200, headers: { "content-type": "text/event-stream" } },
    )) as unknown as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const events: unknown[] = []
  for await (const event of await client.runs.events.subscribe("run-1")) {
    events.push(event)
    break
  }

  expect(events).toEqual([
    {
      type: "waitpoint",
      run_id: "run-1",
      waitpoint_id: "token-1",
      kind: "token",
      params: { subject: "deploy" },
      metadata: {},
      tags: [],
      at: "2026-04-20T00:00:00Z",
    },
  ])
})

test("runs.events.subscribe rejects malformed SSE frames", async () => {
  const encoder = new TextEncoder()
  globalThis.fetch = (async () =>
    new Response(
      new ReadableStream({
        start(controller) {
          controller.enqueue(encoder.encode("data: {not-json}\n\n"))
          controller.close()
        },
      }),
      { status: 200, headers: { "content-type": "text/event-stream" } },
    )) as unknown as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })

  await expect((async () => {
    for await (const _event of await client.runs.events.subscribe("run-1")) {
      // Malformed JSON must fail the stream before any user-facing event is yielded.
    }
  })()).rejects.toThrow("SSE event data must be valid JSON")
})

test("runs.events.subscribe skips malformed SSE frames with a cursor", async () => {
  const encoder = new TextEncoder()
  const requestedUrls: string[] = []
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = String(input)
    requestedUrls.push(url)
    if (url.endsWith("/api/runs/run-1")) {
      return Response.json({
        id: "run-1",
        task_id: "inspect",
        status: "running",
        exit_code: null,
      })
    }
    const event = url.endsWith("cursor=1")
      ? {
          id: "2",
          kind: "run.completed",
          message: "run.completed",
          at: "2026-04-20T00:00:01Z",
          attributes: { run_id: "run-1", exit_code: 0 },
        }
      : undefined
    return new Response(
      new ReadableStream({
        start(controller) {
          if (event === undefined) {
            controller.enqueue(encoder.encode("id: 1\ndata: {not-json}\n\n"))
          } else {
            controller.enqueue(encoder.encode(`data: ${JSON.stringify(event)}\n\n`))
          }
          controller.close()
        },
      }),
      { status: 200, headers: { "content-type": "text/event-stream" } },
    )
  }) as unknown as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const events: unknown[] = []
  for await (const event of await client.runs.events.subscribe("run-1")) {
    events.push(event)
  }

  expect(events).toEqual([
    {
      type: "task_result",
      run_id: "run-1",
      exit_code: 0,
      at: "2026-04-20T00:00:01Z",
    },
  ])
  expect(requestedUrls).toEqual([
    "https://api.example.test/api/runs/run-1/events?follow=1",
    "https://api.example.test/api/runs/run-1",
    "https://api.example.test/api/runs/run-1/events?follow=1&cursor=1",
  ])
})

test("runs.events.subscribe flushes the final SSE frame at EOF", async () => {
  const encoder = new TextEncoder()
  const event = {
    id: "1",
    kind: "audit",
    message: "waitpoint.created",
    at: "2026-04-20T00:00:00Z",
      attributes: {
        run_id: "run-1",
        kind: "token",
        waitpoint_id: "token-1",
        params: { type: "note" },
    },
  }
  globalThis.fetch = (async () =>
    new Response(
      new ReadableStream({
        start(controller) {
          controller.enqueue(encoder.encode(`data: ${JSON.stringify(event)}`))
          controller.close()
        },
      }),
      { status: 200, headers: { "content-type": "text/event-stream" } },
    )) as unknown as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const events: unknown[] = []
  for await (const event of await client.runs.events.subscribe("run-1")) {
    events.push(event)
    break
  }

  expect(events).toEqual([
    {
      type: "waitpoint",
      run_id: "run-1",
      waitpoint_id: "token-1",
      kind: "token",
      params: { type: "note" },
      metadata: {},
      tags: [],
      at: "2026-04-20T00:00:00Z",
    },
  ])
})

test("runs.events.subscribe reconnects with the last event cursor and ends after a terminal event", async () => {
  const encoder = new TextEncoder()
  const requestedUrls: string[] = []
  const first = {
    id: "1",
    kind: "audit",
    message: "waitpoint.created",
    at: "2026-04-20T00:00:00Z",
      attributes: {
        run_id: "run-1",
        kind: "token",
        waitpoint_id: "token-1",
        params: {},
    },
  }
  const second = {
    id: "2",
    kind: "run.completed",
    message: "run.completed",
    at: "2026-04-20T00:00:01Z",
    attributes: {
      run_id: "run-1",
      exit_code: 0,
    },
  }
  const eventResponses = [first, second]
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = String(input)
    requestedUrls.push(url)
    if (url.endsWith("/api/runs/run-1")) {
      return Response.json({
        id: "run-1",
        task_id: "inspect",
        status: "running",
        exit_code: null,
      })
    }
    const event = eventResponses.shift()
    return new Response(
      new ReadableStream({
        start(controller) {
          if (event !== undefined) {
            controller.enqueue(encoder.encode(`data: ${JSON.stringify(event)}\n\n`))
          }
          controller.close()
        },
      }),
      { status: 200, headers: { "content-type": "text/event-stream" } },
    )
  }) as unknown as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const events: unknown[] = []
  for await (const event of await client.runs.events.subscribe("run-1")) {
    events.push(event)
  }

  expect(requestedUrls).toEqual([
    "https://api.example.test/api/runs/run-1/events?follow=1",
    "https://api.example.test/api/runs/run-1",
    "https://api.example.test/api/runs/run-1/events?follow=1&cursor=1",
  ])
  expect(events).toEqual([
    {
      type: "waitpoint",
      run_id: "run-1",
      waitpoint_id: "token-1",
      kind: "token",
      params: {},
      metadata: {},
      tags: [],
      at: "2026-04-20T00:00:00Z",
    },
    {
      type: "task_result",
      run_id: "run-1",
      exit_code: 0,
      at: "2026-04-20T00:00:01Z",
    },
  ])
})

test("runs.events.subscribe drains remaining events when a clean disconnect finds a terminal snapshot", async () => {
  const requestedUrls: string[] = []
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = String(input)
    requestedUrls.push(url)
    if (url.endsWith("/events?follow=1&cursor=2")) {
      return new Response(new ReadableStream({ start: (controller) => controller.close() }), {
        status: 200,
        headers: { "content-type": "text/event-stream" },
      })
    }
    if (url.endsWith("/events?cursor=2")) {
      return Response.json({
        events: [
          {
            id: "3",
            kind: "run.completed",
            message: "run.completed",
            at: "2026-04-20T00:00:01Z",
            attributes: {
              run_id: "run-1",
              exit_code: 0,
            },
          },
        ],
        cursor: 2,
        next_cursor: null,
      })
    }
    return Response.json({
      id: "run-1",
      task_id: "inspect",
      status: "succeeded",
      exit_code: 0,
    })
  }) as unknown as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const events: unknown[] = []
  for await (const event of await client.runs.events.subscribe("run-1", { cursor: 2 })) {
    events.push(event)
  }

  expect(events).toEqual([
    {
      type: "task_result",
      run_id: "run-1",
      exit_code: 0,
      at: "2026-04-20T00:00:01Z",
    },
  ])
  expect(requestedUrls).toEqual([
    "https://api.example.test/api/runs/run-1/events?follow=1&cursor=2",
    "https://api.example.test/api/runs/run-1",
    "https://api.example.test/api/runs/run-1/events?cursor=2",
  ])
})

test("runs.retrieve returns a run snapshot with a discriminated pending waitpoint", async () => {
  globalThis.fetch = (async (_input: RequestInfo | URL, _init?: RequestInit) =>
    Response.json({
      id: "run-1",
      task_id: "inspect",
      status: "waiting",
      exit_code: null,
      pending_waitpoint: {
        kind: "token",
        id: "token-1",
        status: "pending",
        params: { action: "deploy" },
        metadata: { bridge: "slack" },
        tags: ["approval"],
        timeout: 30 * 60,
        created_at: "2026-04-20T00:00:00Z",
      },
    })) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const run = await client.runs.retrieve("run-1")

  if (run.pendingWaitpoint) {
    expect(run.pendingWaitpoint.runId).toBe("run-1")
    expect(run.pendingWaitpoint.id).toBe("token-1")
    expect(run.pendingWaitpoint.kind).toBe("token")
    expect(run.pendingWaitpoint.status).toBe("pending")
    expect(run.pendingWaitpoint.params).toEqual({ action: "deploy" })
    expect(run.pendingWaitpoint.metadata).toEqual({ bridge: "slack" })
    expect(run.pendingWaitpoint.tags).toEqual(["approval"])
  } else {
    throw new Error("expected pending waitpoint")
  }
})

test("runs.wait follows events and treats succeeded as terminal", async () => {
  const requestedUrls: string[] = []
  const encoder = new TextEncoder()
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = String(input)
    requestedUrls.push(url)
    if (url.endsWith("/events?follow=1")) {
      return new Response(
        new ReadableStream({
          start(controller) {
            controller.enqueue(encoder.encode(`data: ${JSON.stringify({
              id: "1",
              kind: "run.completed",
              message: "run.completed",
              at: "2026-04-20T00:00:01Z",
              attributes: { run_id: "run-1", exit_code: 0 },
            })}\n\n`))
            controller.close()
          },
        }),
        { status: 200, headers: { "content-type": "text/event-stream" } },
      )
    }
    return Response.json({
      id: "run-1",
      task_id: "inspect",
      status: requestedUrls.filter((requested) => requested.endsWith("/api/runs/run-1")).length === 1
        ? "running"
        : "succeeded",
      exit_code: 0,
    })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const finished = await client.runs.wait(
    { id: "run-1", taskId: "inspect" },
    { timeoutMs: 2_000 },
  )

  expect(finished.status).toBe("succeeded")
  expect(requestedUrls).toEqual([
    "https://api.example.test/api/runs/run-1",
    "https://api.example.test/api/runs/run-1/events?follow=1",
    "https://api.example.test/api/runs/run-1",
  ])
})

test("runs.wait checks the snapshot after a clean event stream disconnect", async () => {
  const requestedUrls: string[] = []
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = String(input)
    requestedUrls.push(url)
    if (url.endsWith("/events?follow=1")) {
      return new Response(new ReadableStream({ start: (controller) => controller.close() }), {
        status: 200,
        headers: { "content-type": "text/event-stream" },
      })
    }
    return Response.json({
      id: "run-1",
      task_id: "inspect",
      status: requestedUrls.filter((requested) => requested.endsWith("/api/runs/run-1")).length === 1
        ? "running"
        : "succeeded",
      exit_code: 0,
    })
  }) as unknown as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const finished = await client.runs.wait("run-1", { timeoutMs: 2_000 })

  expect(finished.status).toBe("succeeded")
  expect(requestedUrls).toEqual([
    "https://api.example.test/api/runs/run-1",
    "https://api.example.test/api/runs/run-1/events?follow=1",
    "https://api.example.test/api/runs/run-1",
  ])
})

test("runs.wait reconnects after a transient snapshot error", async () => {
  const requestedUrls: string[] = []
  const responses = [
    Response.json({
      id: "run-1",
      task_id: "inspect",
      status: "running",
      exit_code: null,
    }),
    new Response("try again", { status: 502 }),
    Response.json({
      id: "run-1",
      task_id: "inspect",
      status: "succeeded",
      exit_code: 0,
    }),
  ]
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = String(input)
    requestedUrls.push(url)
    if (url.endsWith("/events?follow=1")) {
      return new Response(new ReadableStream({ start: (controller) => controller.close() }), {
        status: 200,
        headers: { "content-type": "text/event-stream" },
      })
    }
    const response = responses.shift()
    if (response === undefined) {
      throw new Error("unexpected fetch")
    }
    return response
  }) as unknown as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const finished = await client.runs.wait("run-1", { timeoutMs: 3_000 })

  expect(finished.status).toBe("succeeded")
  expect(requestedUrls).toEqual([
    "https://api.example.test/api/runs/run-1",
    "https://api.example.test/api/runs/run-1/events?follow=1",
    "https://api.example.test/api/runs/run-1",
    "https://api.example.test/api/runs/run-1/events?follow=1",
    "https://api.example.test/api/runs/run-1",
  ])
})

test("runs.wait falls back to the snapshot after a malformed event stream", async () => {
  const encoder = new TextEncoder()
  const requestedUrls: string[] = []
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = String(input)
    requestedUrls.push(url)
    if (url.endsWith("/events?follow=1")) {
      return new Response(
        new ReadableStream({
          start(controller) {
            controller.enqueue(encoder.encode("data: {not-json}\n\n"))
            controller.close()
          },
        }),
        { status: 200, headers: { "content-type": "text/event-stream" } },
      )
    }
    return Response.json({
      id: "run-1",
      task_id: "inspect",
      status: requestedUrls.filter((requested) => requested.endsWith("/api/runs/run-1")).length === 1
        ? "running"
        : "succeeded",
      exit_code: 0,
    })
  }) as unknown as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const finished = await client.runs.wait("run-1", { timeoutMs: 2_000 })

  expect(finished.status).toBe("succeeded")
  expect(requestedUrls).toEqual([
    "https://api.example.test/api/runs/run-1",
    "https://api.example.test/api/runs/run-1/events?follow=1",
    "https://api.example.test/api/runs/run-1",
  ])
})

test("runs.wait aborts an in-flight retrieve when timeout elapses", async () => {
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) =>
    await new Promise<Response>((_resolve, reject) => {
      const signal = init?.signal
      if (signal?.aborted === true) {
        reject(signal.reason)
        return
      }
      signal?.addEventListener("abort", () => reject(signal.reason), { once: true })
    })) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })

  await expect(client.runs.wait("run-1", { timeoutMs: 10 })).rejects.toThrow(
    "run run-1 did not finish within 10ms",
  )
})

function taskStartFixture(run: {
  readonly id: string
  readonly task_id: string
  readonly status: string
  readonly [key: string]: unknown
}, opts: { readonly isCached?: boolean } = {}) {
  return {
    session: taskSessionFixture({
      id: "session-1",
      task_id: run.task_id,
      current_run_id: run.id,
      status: "open",
    }),
    run,
    ...(opts.isCached === undefined ? {} : { is_cached: opts.isCached }),
  }
}

function taskSessionFixture(overrides: Partial<{
  readonly id: string
  readonly project_id: string
  readonly environment_id: string
  readonly task_id: string
  readonly initial_deployment_id: string
  readonly active_deployment_id: string
  readonly type: string
  readonly external_id: string
  readonly status: string
  readonly current_run_id: string | null
  readonly workspace_id: string | null
  readonly metadata: Record<string, unknown>
  readonly tags: readonly string[]
  readonly result: unknown
  readonly error: unknown
  readonly timed_out: boolean
  readonly terminal_reason: unknown
  readonly expires_at: string | null
  readonly created_at: string
  readonly updated_at: string
}> = {}) {
  return {
    id: "session-1",
    project_id: "project-1",
    environment_id: "env-1",
    task_id: "inspect",
    initial_deployment_id: "deployment-1",
    active_deployment_id: "deployment-1",
    type: "default",
    status: "open",
    current_run_id: "run-1",
    workspace_id: "workspace-1",
    metadata: {},
    tags: [],
    expires_at: null,
    created_at: "2026-04-20T00:00:00Z",
    updated_at: "2026-04-20T00:00:00Z",
    ...overrides,
  }
}

function workspaceFixture(overrides: Partial<{
  readonly id: string
  readonly project_id: string
  readonly environment_id: string
  readonly deployment_sandbox_id: string
  readonly sandbox_id: string
  readonly sandbox_fingerprint: string
  readonly external_id: string
  readonly current_version_id: string | null
  readonly state: string
  readonly desired_state: string
  readonly dirty_state: string
  readonly last_materialization_id: string | null
  readonly metadata: Record<string, unknown>
  readonly tags: readonly string[]
  readonly last_activity_at: string
  readonly created_at: string
  readonly updated_at: string
}> = {}) {
  return {
    id: "workspace-1",
    project_id: "project-1",
    environment_id: "env-1",
    deployment_sandbox_id: "sandbox-deployment-1",
    sandbox_id: "sandbox-1",
    sandbox_fingerprint: "sandbox-fingerprint-1",
    current_version_id: null,
    state: "active",
    desired_state: "active",
    dirty_state: "clean",
    last_materialization_id: null,
    metadata: {},
    tags: [],
    last_activity_at: "2026-04-20T00:00:00Z",
    created_at: "2026-04-20T00:00:00Z",
    updated_at: "2026-04-20T00:00:00Z",
    ...overrides,
  }
}

function workspaceMaterializationFixture(overrides: Partial<{
  readonly id: string
  readonly project_id: string
  readonly environment_id: string
  readonly workspace_id: string
  readonly deployment_sandbox_id: string
  readonly base_version_id: string | null
  readonly worker_instance_id: string | null
  readonly state: string
  readonly fencing_generation: number
  readonly dirty_generation: number
  readonly reservation_expires_at: string | null
  readonly last_heartbeat_at: string | null
  readonly created_at: string
  readonly updated_at: string
}> = {}) {
  return {
    id: "materialization-1",
    project_id: "project-1",
    environment_id: "env-1",
    workspace_id: "workspace-1",
    deployment_sandbox_id: "sandbox-deployment-1",
    base_version_id: null,
    worker_instance_id: null,
    state: "requested",
    fencing_generation: 1,
    dirty_generation: 0,
    reservation_expires_at: null,
    last_heartbeat_at: null,
    created_at: "2026-04-20T00:00:00Z",
    updated_at: "2026-04-20T00:00:00Z",
    ...overrides,
  }
}

function channelRecordFixture(overrides: Partial<{
  readonly id: string
  readonly channel_id: string
  readonly sequence: number
  readonly data: unknown
  readonly correlation_id: string
  readonly content_type: string
  readonly created_at: string
}> = {}) {
  return {
    id: "record-1",
    channel_id: "channel-1",
    sequence: 1,
    data: null,
    content_type: "application/json",
    created_at: "2026-04-20T00:00:00Z",
    ...overrides,
  }
}
