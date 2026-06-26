import { afterEach, expect, test } from "bun:test"

import { enterRunRuntime, parseTaskPayload, type RunRuntime } from "./internal"
import { PayloadSchemaValidationError, WaitTimeoutError, image, queue, sandbox, schedules, streams, task, type PayloadSchema } from "./index"
import { resetDefaultClientForTest } from "./start"

const originalFetch = globalThis.fetch
const originalEnv = { ...process.env }

afterEach(() => {
  globalThis.fetch = originalFetch
  process.env = { ...originalEnv }
  resetDefaultClientForTest()
})

const invalidTaskIds = [
  "a/b",
  "a?b",
  "a#b",
  "a%b",
  "a b",
  "å",
  "-foo",
  ".foo",
  "",
  "a".repeat(129),
] as const

const sb = sandbox("task-id").image(image("task-id").from("debian:trixie-slim"))

test("task rejects invalid ids through the schema validator", () => {
  for (const id of invalidTaskIds) {
    expect(() => task({ id, sandbox: sb, run: async () => null })).toThrow("task id must match")
  }
})

test("task accepts task id boundaries", () => {
  expect(task({ id: "a", sandbox: sb, run: async () => null }).id).toBe("a")
  const max = "a".repeat(128)
  expect(task({ id: max, sandbox: sb, run: async () => null }).id).toBe(max)
  expect(task({ id: "tokens.grant", sandbox: sb, run: async () => null }).id).toBe("tokens.grant")
})

test("task rejects zero queue concurrency limit", () => {
  expect(() =>
    task({
      id: "zero-queue-limit",
      sandbox: sb,
      queue: queue({ id: "task/zero-queue-limit", concurrencyLimit: 0 }),
      run: async () => null,
    }),
  ).toThrow("queue concurrencyLimit must be a positive integer")
})

test("task rejects invalid ttl strings", () => {
  expect(() =>
    task({
      id: "invalid-ttl",
      sandbox: sb,
      ttl: "10minutes",
      run: async () => null,
    }),
  ).toThrow("ttl must be a positive duration string")
  expect(() =>
    task({
      id: "tiny-ttl",
      sandbox: sb,
      ttl: "0.1ns",
      run: async () => null,
    }),
  ).toThrow("ttl must be a positive duration string")
})

test("task accepts server duration ttl strings", () => {
  expect(task({ id: "plus-ttl", sandbox: sb, ttl: "+1h", run: async () => null }).ttl).toBe("+1h")
  expect(task({ id: "zero-component-ttl", sandbox: sb, ttl: "1h0m", run: async () => null }).ttl).toBe("1h0m")
})

test("task validates retry policies", () => {
  expect(task({
    id: "retrying-task",
    sandbox: sb,
    retry: { maxAttempts: 3, backoff: { minMs: 1000, maxMs: 30000, factor: 2, jitter: "full" } },
    run: async () => null,
  }).retry).toEqual({ maxAttempts: 3, backoff: { minMs: 1000, maxMs: 30000, factor: 2, jitter: "full" } })
  expect(() =>
    task({
      id: "bad-retry-attempts",
      sandbox: sb,
      retry: { maxAttempts: 0 },
      run: async () => null,
    }),
  ).toThrow("retry.maxAttempts must be an integer between 1 and 10")
  expect(() =>
    task({
      id: "bad-retry-field",
      sandbox: sb,
      retry: { maxAttempts: 2, retryOn: ["timeout"] } as never,
      run: async () => null,
    }),
  ).toThrow("retry.retryOn is not supported")
})

function assertTaskConfigRejectsStreams(): void {
  const report = streams.output("report", { schema: issueStringToNumberSchema() })
  task({
    id: "stream-config-rejected",
    sandbox: sb,
    // @ts-expect-error streams are module-level primitives, not task config.
    streams: [report],
    run: async () => null,
  })
}

test("task rejects streams config at runtime", () => {
  const report = streams.output("runtime-report", { schema: issueStringToNumberSchema() })
  expect(() =>
    task({
      id: "stream-config-runtime-rejected",
      sandbox: sb,
      streams: [report],
      run: async () => null,
    } as never),
  ).toThrow("task streams are defined with the module-level streams primitive, not task config")
})

test("task parses payload through payload schema before run", async () => {
  const payload: PayloadSchema<{ readonly issue: string }, { readonly issue: number }> = {
    "~standard": {
      version: 1,
      vendor: "test",
      validate(value) {
        if (value === null || typeof value !== "object") {
          return { issues: [{ message: "expected object" }] }
        }
        const issue = (value as Record<string, unknown>)["issue"]
        if (typeof issue !== "string") {
          return { issues: [{ message: "expected string", path: ["issue"] }] }
        }
        return { value: { issue: Number(issue) } }
      },
    },
  }
  const schemaTask = task({
    id: "payload-schema",
    sandbox: sb,
    payload,
    run: async (payload) => payload.issue + 1,
  })

  await expect(parseTaskPayload(schemaTask, { issue: "41" })).resolves.toEqual({ issue: 41 })
})

test("output stream parses payload before appending", async () => {
  const appended: unknown[] = []
  const exit = enterRunRuntime(fakeRunRuntime({
    outputStreamAppend: async (_stream, payload) => {
      appended.push(payload)
    },
  }))
  try {
    const issueStream = streams.output("issues", { schema: issueStringToNumberSchema() })
    await issueStream.append({ issue: "41" })
    await issueStream.pipe([{ issue: "42" }])

    expect(appended).toEqual([{ issue: 41 }, { issue: 42 }])
    await expect(issueStream.append({ issue: 41 } as never)).rejects.toThrow(PayloadSchemaValidationError)
  } finally {
    exit()
  }
})

test("runtime stream helpers allow repeated schema-bearing handles", async () => {
  const appended: unknown[] = []
  const exit = enterRunRuntime(fakeRunRuntime({
    outputStreamAppend: async (_stream, payload) => {
      appended.push(payload)
    },
  }))
  try {
    await streams.output("runtime-issues", { schema: issueStringToNumberSchema() }).append({ issue: "41" })
    await streams.output("runtime-issues", { schema: issueStringToNumberSchema() }).append({ issue: "42" })

    expect(appended).toEqual([{ issue: 41 }, { issue: 42 }])
  } finally {
    exit()
  }
})

test("input stream on repeats waits and passes payloads to the handler", async () => {
  const payloads = [{ issue: 41 }, { issue: 42 }]
  const seen: unknown[] = []
  const exit = enterRunRuntime(fakeRunRuntime({
    inputStreamPeek: async () => {
      const data = payloads.shift()
      if (data === undefined) {
        throw new Error("done")
      }
      return {
        id: `record-${seen.length + 1}`,
        streamId: "issues",
        sequence: seen.length + 1,
        data,
        contentType: "application/json",
        createdAt: "2026-04-20T00:00:00Z",
      }
    },
  }))
  try {
    await expect(streams.input("issues").on(async (payload) => {
      seen.push(payload)
    })).rejects.toThrow("done")
    expect(seen).toEqual([{ issue: 41 }, { issue: 42 }])
  } finally {
    exit()
  }
})

test("input stream on terminates on explicit timeout", async () => {
  const exit = enterRunRuntime(fakeRunRuntime({
    inputStreamPeek: async () => null,
  }))
  try {
    await expect(streams.input("issues").on(async () => {
      throw new Error("handler should not run")
    }, { timeout: "1s" })).rejects.toThrow(WaitTimeoutError)
  } finally {
    exit()
  }
})

test("input stream on does not apply timeout to every quiet window", async () => {
  const seen: unknown[] = []
  let calls = 0
  const exit = enterRunRuntime(fakeRunRuntime({
    inputStreamPeek: async () => {
      calls += 1
      if (calls === 1) {
        return {
          id: "record-1",
          streamId: "issues",
          sequence: 1,
          data: { issue: 41 },
          contentType: "application/json",
          createdAt: "2026-04-20T00:00:00Z",
        }
      }
      if (calls === 2) {
        return null
      }
      throw new Error("done")
    },
  }))
  try {
    await expect(streams.input("issues").on(async (payload) => {
      seen.push(payload)
    }, { timeout: "1s" })).rejects.toThrow("done")
    expect(seen).toEqual([{ issue: 41 }])
  } finally {
    exit()
  }
})

test("streams reject names that cannot round-trip through the REST path", () => {
  for (const id of ["release/approval", "release approval", "release%2Fapproval", ".approval", "_approval", "-approval", "å"]) {
    expect(() => streams.input(id, { schema: issueStringToNumberSchema() })).toThrow("stream name must match")
  }
  expect(streams.input("release.approval", { schema: issueStringToNumberSchema() }).id).toBe("release.approval")
})

test("task payload schema failures are reported with issue paths", async () => {
  const payload: PayloadSchema<{ readonly issue: string }, { readonly issue: number }> = {
    "~standard": {
      version: 1,
      vendor: "test",
      validate() {
        return { issues: [{ message: "expected string", path: ["issue"] }] }
      },
    },
  }
  const schemaTask = task({
    id: "payload-schema-failure",
    sandbox: sb,
    payload,
    run: async (payload) => payload.issue,
  })

  await expect(parseTaskPayload(schemaTask, { issue: 41 })).rejects.toThrow(PayloadSchemaValidationError)
  await expect(parseTaskPayload(schemaTask, { issue: 41 })).rejects.toThrow(
    'task "payload-schema-failure" payload failed validation: payload.issue: expected string',
  )
})

test("task payload schema paths render numeric string indexes as array indexes", async () => {
  const payload: PayloadSchema<unknown, unknown> = {
    "~standard": {
      version: 1,
      vendor: "test",
      validate() {
        return { issues: [{ message: "expected string", path: ["items", "0", "name"] }] }
      },
    },
  }
  const schemaTask = task({
    id: "payload-schema-index-path",
    sandbox: sb,
    payload,
    run: async (payload) => payload,
  })

  await expect(parseTaskPayload(schemaTask, {})).rejects.toThrow("payload.items[0].name: expected string")
})

test("task accepts callable payload schemas", async () => {
  const callableSchema = Object.assign(
    () => undefined,
    {
      "~standard": {
        version: 1,
        vendor: "test",
        validate(value: unknown) {
          return { value }
        },
      },
    } satisfies PayloadSchema<unknown, unknown>,
  )

  expect(() =>
    task({
      id: "callable-payload-schema",
      sandbox: sb,
      payload: callableSchema,
      run: async (payload) => payload,
    }),
  ).not.toThrow()
})

test("task without payload does not accept parsed payload", async () => {
  const noPayloadTask = task({
    id: "no-payload",
    sandbox: sb,
    run: async (ctx) => ctx.run.id,
  })

  await expect(parseTaskPayload(noPayloadTask, {})).rejects.toThrow('task "no-payload" does not accept payload')
})

test("scheduled tasks parse metadata payload dates", async () => {
  const scheduled = schedules.task({
    id: "scheduled-payload",
    sandbox: sb,
    cron: "0 9 * * *",
    run: async (payload) => payload.timestamp,
  })

  await expect(parseTaskPayload(scheduled, {
    timestamp: "2026-06-02T00:00:00Z",
    lastTimestamp: "2026-06-01T00:00:00Z",
    timezone: "Asia/Tokyo",
    scheduleId: "schedule-1",
    scheduleType: "declarative",
    externalId: "customer-1",
    upcoming: ["2026-06-03T00:00:00Z"],
  })).resolves.toEqual({
    timestamp: new Date("2026-06-02T00:00:00Z"),
    lastTimestamp: new Date("2026-06-01T00:00:00Z"),
    timezone: "Asia/Tokyo",
    scheduleId: "schedule-1",
    scheduleType: "declarative",
    externalId: "customer-1",
    upcoming: [new Date("2026-06-03T00:00:00Z")],
  })
})

function issueStringToNumberSchema(): PayloadSchema<{ readonly issue: string }, { readonly issue: number }> {
  return {
    "~standard": {
      version: 1,
      vendor: "test",
      validate(value) {
        if (value === null || typeof value !== "object") {
          return { issues: [{ message: "expected object" }] }
        }
        const issue = (value as Record<string, unknown>)["issue"]
        if (typeof issue !== "string") {
          return { issues: [{ message: "expected string", path: ["issue"] }] }
        }
        return { value: { issue: Number(issue) } }
      },
    },
  }
}

function fakeRunRuntime(overrides: Partial<RunRuntime>): RunRuntime {
  return {
    createToken: async () => ({ id: "token-1", status: "pending", callbackUrl: "https://api.example.test/api/tokens/token-1/callback/callback-secret", timeoutAt: null, wait: () => { throw new Error("not implemented") } }),
    waitToken: async () => ({ ok: true, data: undefined, unwrap: () => undefined as never }),
    inputStreamWait: async () => ({ ok: true, data: undefined, unwrap: () => undefined as never }),
    inputStreamOnce: async () => ({ ok: true, data: undefined, unwrap: () => undefined as never }),
    inputStreamPeek: async () => null,
    outputStreamAppend: async () => {},
    outputStreamRead: async () => null,
    outputStreamList: async () => [],
    waitFor: async () => {},
    waitUntil: async () => {},
    metadataSet: async () => {},
    metadataPatch: async () => {},
    metadataIncrement: async () => {},
    log: () => {},
    ...overrides,
  }
}
