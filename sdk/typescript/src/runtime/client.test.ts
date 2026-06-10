import { afterEach, expect, test } from "bun:test"

import { HelmrClient } from "./client"
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

test("https transport can be initialized without an api key", async () => {
  delete process.env["HELMR_API_KEY"]
  let authorization: string | null | undefined
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    authorization = new Headers(init?.headers).get("authorization")
    return new Response(null, { status: 204 })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test" })
  await client.waitpoints.tokens.respond("token-id", "raw-token", { value: { ok: true } })

  expect(authorization).toBeNull()
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
      deploymentId: "deployment-1",
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
      deployment_id: "deployment-1",
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

test("task.trigger returns a run handle from the create-run response", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  process.env["HELMR_API_KEY"] = "token"
  let requestedUrl: string | undefined
  let body: unknown
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrl = String(input)
    body = JSON.parse(String(init?.body))
    return Response.json(
      {
        id: "018f0000000070008000000000000001",
        task_id: "inspect",
        status: "running",
      },
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
  const handle = await inspect.trigger({
  })

  expect(requestedUrl).toBe("https://api.example.test/api/runs")
  expect(body).toEqual({
    task_id: "inspect",
    options: { max_duration_seconds: 900 },
  })
  expect(handle).toEqual({ id: "018f0000000070008000000000000001", taskId: "inspect" })
})

test("task.trigger posts idempotency options", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  let body: unknown
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    body = JSON.parse(String(init?.body))
    return Response.json({ id: "run-1", task_id: "inspect", status: "queued" }, { status: 201 })
  }) as typeof fetch

  const key = idempotencyKeys.create(["deploy", "prod"], { scope: "global" })
  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    run: async () => undefined,
  })
  await inspect.trigger({
    idempotencyKey: key,
    idempotencyKeyTTL: "24h",
  })

  expect(body).toMatchObject({
    task_id: "inspect",
    options: {
      idempotency_key: key.value,
      idempotency_key_ttl: "24h",
      idempotency_key_options: {
        key: ["deploy", "prod"],
        scope: "global",
      },
      max_duration_seconds: 900,
    },
  })
})

test("attempt-scoped idempotency keys require attempt identity", () => {
  expect(() => idempotencyKeys.create("deploy", { scope: "attempt" } as never)).toThrow("runId and attemptNumber")
})

test("task.trigger posts scheduling options", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  let body: unknown
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    body = JSON.parse(String(init?.body))
    return Response.json({ id: "run-1", task_id: "inspect", status: "queued" }, { status: 201 })
  }) as typeof fetch

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    run: async () => undefined,
  })
  await inspect.trigger({
    queue: "review/pr",
    concurrencyKey: "repo:42",
    priority: 30,
    ttl: "10m",
  })

  expect(body).toMatchObject({
    task_id: "inspect",
    options: {
      queue: { name: "review/pr" },
      concurrency_key: "repo:42",
      priority: 30,
      ttl: "10m",
      max_duration_seconds: 900,
    },
  })
})

test("task.trigger posts retry metadata and tags", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  let body: unknown
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    body = JSON.parse(String(init?.body))
    return Response.json({ id: "run-1", task_id: "inspect", status: "queued" }, { status: 201 })
  }) as typeof fetch

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    run: async () => undefined,
  })
  await inspect.trigger({
    retry: { maxAttempts: 3, backoff: { minMs: 1000, maxMs: 30000, factor: 2, jitter: "full" } },
    metadata: { customer: "acme" },
    tags: ["nightly", "prod"],
  })

  expect(body).toMatchObject({
    task_id: "inspect",
    options: {
      retry: { maxAttempts: 3, backoff: { minMs: 1000, maxMs: 30000, factor: 2, jitter: "full" } },
      metadata: { customer: "acme" },
      tags: ["nightly", "prod"],
      max_duration_seconds: 900,
    },
  })
})

test("task.trigger rejects unsupported retry fields before posting", async () => {
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

  await expect(inspect.trigger({
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

test("runs.replay posts replay intent and returns the new run handle", async () => {
  let requestedUrl: string | undefined
  let body: unknown
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrl = String(input)
    body = JSON.parse(String(init?.body))
    return Response.json({
      run: {
        id: "run-2",
        task_id: "inspect",
        status: "queued",
      },
      operation: {
        id: "operation-1",
        run_id: "run-1",
        kind: "replay",
        status: "applied",
        created_at: "2026-01-01T00:00:00Z",
      },
    }, { status: 201 })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const handle = await client.runs.replay<{ env: string }>("run-1", {
    version: "latest",
    payload: { env: "prod" },
    metadata: { replay: true },
    tags: ["manual"],
    reason: "rerun",
    idempotencyKey: "replay-run-1",
  })

  expect(requestedUrl).toBe("https://api.example.test/api/runs/run-1/replay")
  expect(body).toEqual({
    version: "latest",
    payload: { env: "prod" },
    metadata: { replay: true },
    tags: ["manual"],
    reason: "rerun",
    idempotency_key: "replay-run-1",
  })
  expect(handle).toEqual({ id: "run-2", taskId: "inspect" })
})

test("task.trigger validates payload before posting the run", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  process.env["HELMR_API_KEY"] = "token"
  let fetched = false
  globalThis.fetch = (async () => {
    fetched = true
    return Response.json({ id: "run-1", task_id: "inspect", status: "running" }, { status: 201 })
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
  await expect(inspect.trigger(
    { issue: "123" },
    {},
  )).rejects.toThrow(PayloadSchemaValidationError)
  expect(fetched).toBe(false)
})

test("task.trigger rejects undefined payload for schema-backed tasks before posting", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  process.env["HELMR_API_KEY"] = "token"
  let fetched = false
  globalThis.fetch = (async () => {
    fetched = true
    return Response.json({ id: "run-1", task_id: "inspect", status: "running" }, { status: 201 })
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
  await expect((inspect.trigger as (...args: any[]) => Promise<unknown>)(
    undefined,
    {},
  )).rejects.toThrow('task "inspect" requires payload')
  expect(fetched).toBe(false)
})

test("task.trigger rejects payload on no-payload tasks before posting", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  process.env["HELMR_API_KEY"] = "token"
  let fetched = false
  globalThis.fetch = (async () => {
    fetched = true
    return Response.json({ id: "run-1", task_id: "inspect", status: "running" }, { status: 201 })
  }) as typeof fetch

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    run: async () => undefined,
  })
  await expect((inspect.trigger as (...args: any[]) => Promise<unknown>)(
    undefined,
    {},
  )).rejects.toThrow('task "inspect" does not accept payload')
  expect(fetched).toBe(false)
})

test("task.trigger posts payload for schema-backed tasks", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  process.env["HELMR_API_KEY"] = "token"
  let body: unknown
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    body = JSON.parse(String(init?.body))
    return Response.json({ id: "run-1", task_id: "inspect", status: "running" }, { status: 201 })
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
  await inspect.trigger(
    { issue: 123 },
    {},
  )

  expect(body).toMatchObject({ payload: { issue: 123 } })
})

test("task.trigger validates payload and posts through the default client", async () => {
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
    return Response.json({ id: "run-1", task_id: "inspect", status: "running" }, { status: 201 })
  }) as typeof fetch

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    payload,
    run: async (payload) => payload.issue,
  })
  const handle = await inspect.trigger(
    { issue: "123" },
    {},
  )

  expect(requestedUrl).toBe("https://api.example.test/api/runs")
  expect(authorization).toBe("Bearer token")
  expect(body).toMatchObject({
    task_id: "inspect",
    payload: { issue: "123" },
    options: { max_duration_seconds: 900 },
  })
  expect(handle).toEqual({ id: "run-1", taskId: "inspect" })
})

test("client.tasks.trigger posts id-based payload without local schema validation", async () => {
  let validated = false
  let body: unknown
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    body = JSON.parse(String(init?.body))
    return Response.json({ id: "run-1", task_id: "inspect", status: "running" }, { status: 201 })
  }) as typeof fetch
  const payload: PayloadSchema<{ readonly issue: string }, { readonly issue: number }> = {
    "~standard": {
      version: 1,
      vendor: "test",
      validate() {
        validated = true
        return { issues: [{ message: "should not validate id-based triggers" }] }
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
  await client.tasks.trigger<typeof inspect>(
    "inspect",
    { issue: "123" },
    {},
  )

  expect(validated).toBe(false)
  expect(body).toEqual({
    task_id: "inspect",
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

test("task.trigger posts a run without workspace preparation", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  process.env["HELMR_API_KEY"] = "token"
  let body: unknown
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    body = JSON.parse(String(init?.body))
    return Response.json(
      {
        id: "018f0000000070008000000000000003",
        task_id: "inspect",
        status: "running",
      },
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
  await inspect.trigger({})

  expect(body).toMatchObject({
    options: { max_duration_seconds: 900 },
  })
})

test("tasks.trigger leaves build validation to the remote worker", async () => {
  let requestedUrl: string | undefined
  globalThis.fetch = (async (_input: RequestInfo | URL, _init?: RequestInit) => {
    requestedUrl = String(_input)
    return Response.json({ id: "run-1", task_id: "inspect", status: "queued" }, { status: 201 })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(
      image("inspect").from("debian:trixie-slim").copy("/tool.sh", source.file("tool.sh")),
    ),
    run: async () => undefined,
  })
  await client.tasks.trigger<typeof inspect>("inspect", {
  })
  expect(requestedUrl).toBe("https://api.example.test/api/runs")
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
          message: "waitpoint.requested",
          at: "2026-04-28T00:00:00Z",
          attributes: {
            kind: "human",
            waitpoint_id: "token-1",
            display_text: "Approve deploy?",
            timeout: 30,
            request: { message: "Approve deploy?", timeout: 30 },
          },
        },
        {
          id: "2",
          run_id: "run-1",
          kind: "waitpoint",
          message: "waitpoint.requested",
          at: "2026-04-28T00:00:01Z",
          attributes: {
            kind: "human",
            waitpoint_id: "token-2",
            timeout: 60,
            request: { prompt: "What changed?", timeout: 60 },
          },
        },
        {
          id: "3",
          run_id: "run-1",
          kind: "waitpoint",
          message: "waitpoint.resolved",
          at: "2026-04-28T00:00:02Z",
          attributes: {
            kind: "human",
            waitpoint_id: "token-2",
            resolution_kind: "completed",
            principal: "user",
            result: { text: "Dependency update", attachments: [] },
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
      type: "waitpoint_request",
      run_id: "run-1",
      waitpoint_id: "token-1",
      kind: "human",
      displayText: "Approve deploy?",
      request: { message: "Approve deploy?", timeout: 30 },
      timeout: 30,
      at: "2026-04-28T00:00:00Z",
    },
    {
      type: "waitpoint_request",
      run_id: "run-1",
      waitpoint_id: "token-2",
      kind: "human",
      displayText: "",
      request: { prompt: "What changed?", timeout: 60 },
      timeout: 60,
      at: "2026-04-28T00:00:01Z",
    },
    {
      type: "waitpoint_resolved",
      run_id: "run-1",
      waitpoint_id: "token-2",
      kind: "human",
      resolutionKind: "completed",
      value: { text: "Dependency update", attachments: [] },
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

test("task.trigger posts directly without uploading source tar", async () => {
  process.env["HELMR_API_URL"] = "https://api.example.test"
  process.env["HELMR_API_KEY"] = "token"
  const urls: string[] = []
  let createRunBody: unknown
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    urls.push(url)
    createRunBody = JSON.parse(String(init?.body))
    return Response.json(
      {
        id: "018f0000000070008000000000000002",
        task_id: "inspect",
        status: "running",
      },
      { status: 201 },
    )
  }) as typeof fetch

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    run: async () => undefined,
  })
  await inspect.trigger({})

  expect(urls).toEqual(["https://api.example.test/api/runs"])
  expect(createRunBody).toMatchObject({
    task_id: "inspect",
    options: { max_duration_seconds: 900 },
  })
})

test("waitpoints.respond posts to the waitpoint respond route", async () => {
  let requestedUrl: string | undefined
  let body: unknown
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrl = String(input)
    body = JSON.parse(String(init?.body))
    return new Response(null, { status: 204 })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  await client.waitpoints.respond(
    {
      waitpointId: "00000000-0000-0000-0000-000000000002",
      kind: "human",
      request: {},
      displayText: "Continue deploy?",
      timeout: null,
      requestedAt: "2026-04-20T00:00:00Z",
    },
    { value: { approved: true }, metadata: { reason: "looks good" } },
  )

  expect(requestedUrl).toBe(
    "https://api.example.test/api/waitpoints/00000000-0000-0000-0000-000000000002/respond",
  )
  expect(body).toEqual({ value: { approved: true }, metadata: { reason: "looks good" } })
})

test("waitpoints.create posts standalone human waitpoints", async () => {
  let requestedUrl: string | undefined
  let body: unknown
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrl = String(input)
    body = JSON.parse(String(init?.body))
    return Response.json({
      id: "waitpoint-1",
      project_id: "project-1",
      environment_id: "env-1",
      kind: "human",
      status: "pending",
      request: { channel: "approval" },
      display_text: "Continue?",
      expires_at: "2026-04-20T01:00:00Z",
      created_at: "2026-04-20T00:00:00Z",
    })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const waitpoint = await client.waitpoints.create({
    request: { channel: "approval" },
    displayText: "Continue?",
    expiresAt: "2026-04-20T01:00:00Z",
    idempotencyKey: "approval-1",
  })

  expect(requestedUrl).toBe("https://api.example.test/api/waitpoints")
  expect(body).toEqual({
    request: { channel: "approval" },
    display_text: "Continue?",
    expires_at: "2026-04-20T01:00:00Z",
    idempotency_key: "approval-1",
  })
  expect(waitpoint).toEqual({
    id: "waitpoint-1",
    projectId: "project-1",
    environmentId: "env-1",
    kind: "human",
    status: "pending",
    request: { channel: "approval" },
    displayText: "Continue?",
    expiresAt: "2026-04-20T01:00:00Z",
    createdAt: "2026-04-20T00:00:00Z",
  })
})

test("retrieved run snapshots expose data-only human waitpoints", async () => {
  let requestedUrl: string | undefined
  let body: unknown
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === undefined) {
      return Response.json({
        id: "run-1",
        task_id: "inspect",
        status: "waiting",
        exit_code: null,
        pending_waitpoint: {
          kind: "human",
          waitpoint_id: "token-1",
          request: { channel: "deploy-review" },
          display_text: "Continue?",
          requested_at: "2026-04-20T00:00:00Z",
        },
      })
    }
    requestedUrl = String(input)
    body = init?.body === undefined ? undefined : JSON.parse(String(init.body))
    return new Response(null, { status: 204 })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const run = await client.runs.retrieve("run-1")
  if (run.pendingWaitpoint?.kind !== "human") throw new Error("expected human wait")
  await client.waitpoints.respond(run.pendingWaitpoint, { value: { approved: true } })

  expect(requestedUrl).toBe("https://api.example.test/api/waitpoints/token-1/respond")
  expect(body).toEqual({ value: { approved: true } })
})

test("waitpoints.tokens.create posts to the token create route", async () => {
  let requestedUrl: string | undefined
  let body: unknown
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrl = String(input)
    body = JSON.parse(String(init?.body))
    return Response.json(
      {
        id: "token-id",
        waitpoint_id: "waitpoint-1",
        url: "https://api.example.test/waitpoints/respond?id=token-id&token=secret-token",
        token: "secret-token",
        expires_at: "2026-04-20T01:00:00Z",
      },
      { status: 201 },
    )
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const token = await client.waitpoints.tokens.create(
    {
      waitpointId: "waitpoint-1",
      kind: "human",
      request: {},
      displayText: "Continue deploy?",
      timeout: null,
      requestedAt: "2026-04-20T00:00:00Z",
    },
    {
      expiresInSeconds: 3600,
      metadata: { recipient: "reviewer@example.com" },
    },
  )

  expect(requestedUrl).toBe("https://api.example.test/api/waitpoints/tokens")
  expect(body).toEqual({
    waitpoint_id: "waitpoint-1",
    expires_in_seconds: 3600,
    metadata: { recipient: "reviewer@example.com" },
  })
  expect(token).toEqual({
    id: "token-id",
    waitpointId: "waitpoint-1",
    url: "https://api.example.test/waitpoints/respond?id=token-id&token=secret-token",
    token: "secret-token",
    expiresAt: "2026-04-20T01:00:00Z",
  })
})

test("waitpoints.tokens.create accepts explicit waitpoint id", async () => {
  let body: unknown
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    body = JSON.parse(String(init?.body))
    return Response.json({
      id: "token-id",
      run_id: "run-1",
      waitpoint_id: "waitpoint-1",
      url: "https://api.example.test/waitpoints/tokens/token-id",
      token: "secret-token",
    })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  await client.waitpoints.tokens.create("waitpoint-1", {
    expiresAt: "2026-04-20T01:00:00Z",
  })

  expect(body).toEqual({
    waitpoint_id: "waitpoint-1",
    expires_at: "2026-04-20T01:00:00Z",
  })
})

test("waitpoints.tokens.respond posts to the token respond route", async () => {
  let requestedUrl: string | undefined
  let body: unknown
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrl = String(input)
    body = JSON.parse(String(init?.body))
    return new Response(null, { status: 204 })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  await client.waitpoints.tokens.respond({
    id: "token-id",
    waitpointId: "waitpoint-1",
    url: "https://api.example.test/waitpoints/respond?id=token-id&token=raw-token",
    token: "raw-token",
    expiresAt: null,
  }, {
    value: { text: "continue" },
    externalSubject: "reviewer@example.com",
    metadata: { source: "email" },
  })

  expect(requestedUrl).toBe("https://api.example.test/api/waitpoints/tokens/token-id/respond")
  expect(body).toEqual({
    token: "raw-token",
    value: { text: "continue" },
    external_subject: "reviewer@example.com",
    metadata: { source: "email" },
  })
})

test("waitpoints.tokens.respond accepts explicit id and raw token", async () => {
  let requestedUrl: string | undefined
  let body: unknown
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrl = String(input)
    body = JSON.parse(String(init?.body))
    return new Response(null, { status: 204 })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  await client.waitpoints.tokens.respond("token-id", "raw-token", {
    value: { reviewed: true },
  })

  expect(requestedUrl).toBe("https://api.example.test/api/waitpoints/tokens/token-id/respond")
  expect(body).toEqual({
    token: "raw-token",
    value: { reviewed: true },
  })
})

test("runs.events.subscribe handles CRLF SSE frames split across chunks", async () => {
  const encoder = new TextEncoder()
  const event = {
    id: "1",
    kind: "audit",
    message: "waitpoint.requested",
    at: "2026-04-20T00:00:00Z",
    attributes: {
      run_id: "run-1",
      kind: "human",
      waitpoint_id: "token-1",
      display_text: "Continue?",
      request: { subject: "deploy" },
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
  }

  expect(events).toEqual([
    {
      type: "waitpoint_request",
      run_id: "run-1",
      waitpoint_id: "token-1",
      kind: "human",
      displayText: "Continue?",
      request: { subject: "deploy" },
      at: "2026-04-20T00:00:00Z",
    },
  ])
})

test("runs.events.subscribe skips malformed SSE frames and keeps reading", async () => {
  const encoder = new TextEncoder()
  const event = {
    id: "2",
    kind: "audit",
    message: "waitpoint.requested",
    at: "2026-04-20T00:00:01Z",
    attributes: {
      run_id: "run-1",
      kind: "human",
      waitpoint_id: "token-1",
      display_text: "Continue?",
    },
  }
  globalThis.fetch = (async () =>
    new Response(
      new ReadableStream({
        start(controller) {
          controller.enqueue(encoder.encode("data: {not-json}\n\n"))
          controller.enqueue(encoder.encode(`data: ${JSON.stringify(event)}\n\n`))
          controller.close()
        },
      }),
      { status: 200, headers: { "content-type": "text/event-stream" } },
    )) as unknown as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const events: unknown[] = []
  for await (const event of await client.runs.events.subscribe("run-1")) {
    events.push(event)
  }

  expect(events).toEqual([
    {
      type: "waitpoint_request",
      run_id: "run-1",
      waitpoint_id: "token-1",
      kind: "human",
      displayText: "Continue?",
      request: {},
      at: "2026-04-20T00:00:01Z",
    },
  ])
})

test("runs.events.subscribe flushes the final SSE frame at EOF", async () => {
  const encoder = new TextEncoder()
  const event = {
    id: "1",
    kind: "audit",
    message: "waitpoint.requested",
    at: "2026-04-20T00:00:00Z",
    attributes: {
      run_id: "run-1",
      kind: "human",
      waitpoint_id: "token-1",
      display_text: "What changed?",
      request: { type: "note" },
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
  }

  expect(events).toEqual([
    {
      type: "waitpoint_request",
      run_id: "run-1",
      waitpoint_id: "token-1",
      kind: "human",
      displayText: "What changed?",
      request: { type: "note" },
      at: "2026-04-20T00:00:00Z",
    },
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
        kind: "human",
        waitpoint_id: "token-1",
        display_text: "Continue?",
        request: { action: "deploy" },
        timeout: 30 * 60,
        requested_at: "2026-04-20T00:00:00Z",
      },
    })) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const run = await client.runs.retrieve("run-1")

  if (run.pendingWaitpoint?.kind === "human") {
    expect(run.pendingWaitpoint.runId).toBe("run-1")
    expect(run.pendingWaitpoint.waitpointId).toBe("token-1")
    expect(run.pendingWaitpoint.displayText).toBe("Continue?")
    expect(run.pendingWaitpoint.request).toEqual({ action: "deploy" })
  } else {
    throw new Error("expected token pending wait")
  }
})

test("runs.wait accepts a run handle and treats succeeded as terminal", async () => {
  const statuses = ["running", "succeeded"]
  const requestedUrls: string[] = []
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requestedUrls.push(String(input))
    return Response.json({
      id: "run-1",
      task_id: "inspect",
      status: statuses.shift() ?? "succeeded",
      exit_code: 0,
    })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const finished = await client.runs.wait(
    { id: "run-1", taskId: "inspect" },
    { timeoutMs: 2_000, intervalMs: 0 },
  )

  expect(finished.status).toBe("succeeded")
  expect(requestedUrls).toEqual([
    "https://api.example.test/api/runs/run-1",
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

  await expect(client.runs.wait("run-1", { timeoutMs: 10, intervalMs: 0 })).rejects.toThrow(
    "run run-1 did not finish within 10ms",
  )
})
