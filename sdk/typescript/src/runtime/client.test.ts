import { afterEach, expect, test } from "bun:test"

import { HelmrClient } from "./client"
import { runStateBooleans } from "./run"
import { PayloadSchemaValidationError, idempotencyKeys, image, sandbox, source, task, workspace, type PayloadSchema } from "../index"

const originalFetch = globalThis.fetch
const originalEnv = { ...process.env }
const originalWarn = console.warn
const testGitSha = "0123456789abcdef0123456789abcdef01234567"

afterEach(() => {
  globalThis.fetch = originalFetch
  process.env = { ...originalEnv }
  console.warn = originalWarn
})

test("constructor requires url option or HELMR_URL", () => {
  delete process.env["HELMR_URL"]
  delete process.env["HELMR_API_KEY"]

  expect(() => new HelmrClient({})).toThrow("requires a url option or HELMR_URL")
})

test("constructor reads HELMR_URL directly without fromEnv helper", async () => {
  process.env["HELMR_URL"] = "https://api.example.test"
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

test("workspace.github accepts branch, tag, or commit refs", () => {
  expect(workspace.github("helmrdotdev/helmr", { ref: " main " })).toEqual({
    kind: "github",
    repository: "helmrdotdev/helmr",
    ref: "main",
  })
  expect(workspace.github("helmrdotdev/helmr", { ref: "refs/tags/v1.0.0" })).toEqual({
    kind: "github",
    repository: "helmrdotdev/helmr",
    ref: "refs/tags/v1.0.0",
  })
  expect(workspace.github("helmrdotdev/helmr", { ref: testGitSha })).toEqual({
    kind: "github",
    repository: "helmrdotdev/helmr",
    ref: testGitSha,
  })
  expect(() => workspace.github("helmrdotdev/helmr", { ref: "" })).toThrow(
    "workspace.github() ref is required",
  )
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
  process.env["HELMR_URL"] = "https://api.example.test"
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
    secrets: {},
    run: async () => undefined,
  })
  const handle = await inspect.trigger({
    workspace: workspace.github("helmrdotdev/helmr", { ref: "main", subpath: "sdk/typescript" }),
  })

  expect(requestedUrl).toBe("https://api.example.test/api/runs")
  expect(body).toEqual({
    task_id: "inspect",
    secrets: {},
    workspace: {
      repository: "helmrdotdev/helmr",
      ref: "main",
      subpath: "sdk/typescript",
    },
    options: { max_duration_seconds: 900 },
  })
  expect(handle).toEqual({ id: "018f0000000070008000000000000001", taskId: "inspect" })
})

test("task.trigger posts idempotency options", async () => {
  process.env["HELMR_URL"] = "https://api.example.test"
  let body: unknown
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    body = JSON.parse(String(init?.body))
    return Response.json({ id: "run-1", task_id: "inspect", status: "queued" }, { status: 201 })
  }) as typeof fetch

  const key = idempotencyKeys.create(["deploy", "prod"], { scope: "global" })
  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    secrets: {},
    run: async () => undefined,
  })
  await inspect.trigger({
    workspace: workspace.github("helmrdotdev/helmr", { ref: "main" }),
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

test("task.trigger posts scheduling options", async () => {
  process.env["HELMR_URL"] = "https://api.example.test"
  let body: unknown
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    body = JSON.parse(String(init?.body))
    return Response.json({ id: "run-1", task_id: "inspect", status: "queued" }, { status: 201 })
  }) as typeof fetch

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    secrets: {},
    run: async () => undefined,
  })
  await inspect.trigger({
    workspace: workspace.github("helmrdotdev/helmr", { ref: "main" }),
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

test("task.trigger validates payloadSchema before posting the run", async () => {
  process.env["HELMR_URL"] = "https://api.example.test"
  process.env["HELMR_API_KEY"] = "token"
  let fetched = false
  globalThis.fetch = (async () => {
    fetched = true
    return Response.json({ id: "run-1", task_id: "inspect", status: "running" }, { status: 201 })
  }) as typeof fetch
  const payloadSchema: PayloadSchema<{ readonly issue: string }, { readonly issue: number }> = {
    "~standard": {
      version: 1,
      vendor: "test",
      validate() {
        return { issues: [{ message: "expected string", path: ["issue"] }] }
      },
    },
    toJSONSchema() {
      return {}
    },
  }

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    payloadSchema,
    run: async (payload) => payload.issue,
  })
  await expect(inspect.trigger(
    { issue: "123" },
    { workspace: workspace.github("helmrdotdev/helmr", { ref: "main" }) },
  )).rejects.toThrow(PayloadSchemaValidationError)
  expect(fetched).toBe(false)
})

test("task.trigger rejects undefined payload for schema-backed tasks before posting", async () => {
  process.env["HELMR_URL"] = "https://api.example.test"
  process.env["HELMR_API_KEY"] = "token"
  let fetched = false
  globalThis.fetch = (async () => {
    fetched = true
    return Response.json({ id: "run-1", task_id: "inspect", status: "running" }, { status: 201 })
  }) as typeof fetch
  const payloadSchema: PayloadSchema<undefined | { readonly issue: number }, { readonly issue: number }> = {
    "~standard": {
      version: 1,
      vendor: "test",
      validate(value) {
        return { value: value === undefined ? { issue: 0 } : value }
      },
    },
    toJSONSchema() {
      return {}
    },
  }

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    payloadSchema,
    run: async (payload) => payload.issue,
  })
  await expect((inspect.trigger as (...args: any[]) => Promise<unknown>)(
    undefined,
    { workspace: workspace.github("helmrdotdev/helmr", { ref: "main" }) },
  )).rejects.toThrow('task "inspect" requires payload')
  expect(fetched).toBe(false)
})

test("task.trigger rejects payload on no-payload tasks before posting", async () => {
  process.env["HELMR_URL"] = "https://api.example.test"
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
    { workspace: workspace.github("helmrdotdev/helmr", { ref: "main" }) },
  )).rejects.toThrow('task "inspect" does not accept payload')
  expect(fetched).toBe(false)
})

test("task.trigger posts payload for schema-backed tasks", async () => {
  process.env["HELMR_URL"] = "https://api.example.test"
  process.env["HELMR_API_KEY"] = "token"
  let body: unknown
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    body = JSON.parse(String(init?.body))
    return Response.json({ id: "run-1", task_id: "inspect", status: "running" }, { status: 201 })
  }) as typeof fetch
  const payloadSchema: PayloadSchema<{ readonly issue: number }, { readonly issue: number }> = {
    "~standard": {
      version: 1,
      vendor: "test",
      validate(value) {
        return { value: value as { readonly issue: number } }
      },
    },
    toJSONSchema() {
      return {}
    },
  }

  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    payloadSchema,
    run: async (payload) => payload.issue,
  })
  await inspect.trigger(
    { issue: 123 },
    { workspace: workspace.github("helmrdotdev/helmr", { ref: "main" }) },
  )

  expect(body).toMatchObject({ payload: { issue: 123 } })
})

test("task.trigger validates payloadSchema and posts through the default client", async () => {
  process.env["HELMR_URL"] = "https://api.example.test"
  process.env["HELMR_API_KEY"] = "token"
  let requestedUrl: string | undefined
  let authorization: string | null | undefined
  let body: unknown
  const payloadSchema: PayloadSchema<{ readonly issue: string }, { readonly issue: number }> = {
    "~standard": {
      version: 1,
      vendor: "test",
      validate(value) {
        const issue = (value as { readonly issue: string }).issue
        return { value: { issue: Number(issue) } }
      },
    },
    toJSONSchema() {
      return {}
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
    payloadSchema,
    run: async (payload) => payload.issue,
  })
  const handle = await inspect.trigger(
    { issue: "123" },
    { workspace: workspace.github("helmrdotdev/helmr", { ref: "main" }) },
  )

  expect(requestedUrl).toBe("https://api.example.test/api/runs")
  expect(authorization).toBe("Bearer token")
  expect(body).toMatchObject({
    task_id: "inspect",
    payload: { issue: "123" },
    workspace: { repository: "helmrdotdev/helmr", ref: "main" },
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
  const payloadSchema: PayloadSchema<{ readonly issue: string }, { readonly issue: number }> = {
    "~standard": {
      version: 1,
      vendor: "test",
      validate() {
        validated = true
        return { issues: [{ message: "should not validate id-based triggers" }] }
      },
    },
    toJSONSchema() {
      return {}
    },
  }
  const inspect = task({
    id: "inspect",
    sandbox: sandbox("inspect").image(image("inspect").from("debian:trixie-slim")),
    payloadSchema,
    run: async (payload) => payload.issue,
  })

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  await client.tasks.trigger<typeof inspect>(
    "inspect",
    { issue: "123" },
    { workspace: workspace.github("helmrdotdev/helmr", { ref: "main" }) },
  )

  expect(validated).toBe(false)
  expect(body).toEqual({
    task_id: "inspect",
    secrets: {},
    payload: { issue: "123" },
    workspace: { repository: "helmrdotdev/helmr", ref: "main" },
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

test("task.trigger posts workspace.github without local preparation", async () => {
  process.env["HELMR_URL"] = "https://api.example.test"
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
    secrets: {},
    run: async () => undefined,
  })
  await inspect.trigger({
    workspace: workspace.github("helmrdotdev/helmr", { ref: "refs/heads/main" }),
  })

  expect(body).toMatchObject({
    workspace: { repository: "helmrdotdev/helmr", ref: "refs/heads/main" },
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
    secrets: {},
    run: async () => undefined,
  })
  await client.tasks.trigger<typeof inspect>("inspect", {
    workspace: workspace.github("helmrdotdev/helmr", { ref: testGitSha }),
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
            kind: "manual",
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
            kind: "manual",
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
            kind: "manual",
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
          message: "run.timeout",
          at: "2026-04-28T00:00:05Z",
          attributes: {
            kind: "max_duration",
            elapsed_active_secs: 12,
            limit_secs: 30,
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
      kind: "manual",
      displayText: "Approve deploy?",
      request: { message: "Approve deploy?", timeout: 30 },
      timeout: 30,
      at: "2026-04-28T00:00:00Z",
    },
    {
      type: "waitpoint_request",
      run_id: "run-1",
      waitpoint_id: "token-2",
      kind: "manual",
      displayText: "",
      request: { prompt: "What changed?", timeout: 60 },
      timeout: 60,
      at: "2026-04-28T00:00:01Z",
    },
    {
      type: "waitpoint_resolved",
      run_id: "run-1",
      waitpoint_id: "token-2",
      kind: "manual",
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
      type: "run_timeout",
      run_id: "run-1",
      elapsed_secs: 12,
      limit_secs: 30,
      at: "2026-04-28T00:00:05Z",
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

test("task.trigger posts workspace.github directly without uploading source tar", async () => {
  process.env["HELMR_URL"] = "https://api.example.test"
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
    secrets: {},
    run: async () => undefined,
  })
  await inspect.trigger({
    workspace: workspace.github("helmrdotdev/helmr", { ref: testGitSha }),
  })

  expect(urls).toEqual(["https://api.example.test/api/runs"])
  expect(createRunBody).toMatchObject({
    task_id: "inspect",
    workspace: { repository: "helmrdotdev/helmr", ref: testGitSha },
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
      kind: "manual",
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

test("waitpoints.create posts standalone manual waitpoints", async () => {
  let requestedUrl: string | undefined
  let body: unknown
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requestedUrl = String(input)
    body = JSON.parse(String(init?.body))
    return Response.json({
      id: "waitpoint-1",
      project_id: "project-1",
      environment_id: "env-1",
      kind: "manual",
      status: "pending",
      request: { channel: "approval" },
      display_text: "Continue?",
      expires_at: "2026-04-20T01:00:00Z",
      created_at: "2026-04-20T00:00:00Z",
    })
  }) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const waitpoint = await client.waitpoints.create({
    projectId: "project-1",
    environmentId: "env-1",
    request: { channel: "approval" },
    displayText: "Continue?",
    expiresAt: "2026-04-20T01:00:00Z",
    idempotencyKey: "approval-1",
  })

  expect(requestedUrl).toBe("https://api.example.test/api/waitpoints")
  expect(body).toEqual({
    project_id: "project-1",
    environment_id: "env-1",
    request: { channel: "approval" },
    display_text: "Continue?",
    expires_at: "2026-04-20T01:00:00Z",
    idempotency_key: "approval-1",
  })
  expect(waitpoint).toEqual({
    id: "waitpoint-1",
    projectId: "project-1",
    environmentId: "env-1",
    kind: "manual",
    status: "pending",
    request: { channel: "approval" },
    displayText: "Continue?",
    expiresAt: "2026-04-20T01:00:00Z",
    createdAt: "2026-04-20T00:00:00Z",
  })
})

test("retrieved run snapshots expose data-only manual waitpoints", async () => {
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
          kind: "manual",
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
  if (run.pendingWaitpoint?.kind !== "manual") throw new Error("expected manual wait")
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
      kind: "manual",
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
      kind: "manual",
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
      kind: "manual",
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
      kind: "manual",
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
      kind: "manual",
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
      kind: "manual",
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
      kind: "manual",
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
        kind: "manual",
        waitpoint_id: "token-1",
        display_text: "Continue?",
        request: { action: "deploy" },
        timeout: 30 * 60,
        requested_at: "2026-04-20T00:00:00Z",
      },
    })) as typeof fetch

  const client = new HelmrClient({ url: "https://api.example.test", apiKey: "token" })
  const run = await client.runs.retrieve("run-1")

  if (run.pendingWaitpoint?.kind === "manual") {
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
