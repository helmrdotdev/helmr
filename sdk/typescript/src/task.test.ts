import { expect, test } from "bun:test"

import { parseTaskPayload } from "./internal"
import { PayloadSchemaValidationError, image, queue, sandbox, task, type PayloadSchema } from "./index"

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
  expect(task({ id: "approvals.grant", sandbox: sb, run: async () => null }).id).toBe("approvals.grant")
})

test("task rejects zero queue concurrency limit", () => {
  expect(() =>
    task({
      id: "zero-queue-limit",
      sandbox: sb,
      queue: queue({ name: "task/zero-queue-limit", concurrencyLimit: 0 }),
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

test("task parses payload through payload schema before run", async () => {
  const payloadSchema: PayloadSchema<{ readonly issue: string }, { readonly issue: number }> = {
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
    toJSONSchema() {
      return {}
    },
  }
  const schemaTask = task({
    id: "payload-schema",
    sandbox: sb,
    payloadSchema,
    run: async (payload) => payload.issue + 1,
  })

  await expect(parseTaskPayload(schemaTask, { issue: "41" })).resolves.toEqual({ issue: 41 })
})

test("task payload schema failures are reported with issue paths", async () => {
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
  const schemaTask = task({
    id: "payload-schema-failure",
    sandbox: sb,
    payloadSchema,
    run: async (payload) => payload.issue,
  })

  await expect(parseTaskPayload(schemaTask, { issue: 41 })).rejects.toThrow(PayloadSchemaValidationError)
  await expect(parseTaskPayload(schemaTask, { issue: 41 })).rejects.toThrow(
    'task "payload-schema-failure" payload failed validation: payload.issue: expected string',
  )
})

test("task payload schema paths render numeric string indexes as array indexes", async () => {
  const payloadSchema: PayloadSchema<unknown, unknown> = {
    "~standard": {
      version: 1,
      vendor: "test",
      validate() {
        return { issues: [{ message: "expected string", path: ["items", "0", "name"] }] }
      },
    },
    toJSONSchema() {
      return {}
    },
  }
  const schemaTask = task({
    id: "payload-schema-index-path",
    sandbox: sb,
    payloadSchema,
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
      toJSONSchema() {
        return {}
      },
    } satisfies PayloadSchema<unknown, unknown>,
  )

  expect(() =>
    task({
      id: "callable-payload-schema",
      sandbox: sb,
      payloadSchema: callableSchema,
      run: async (payload) => payload,
    }),
  ).not.toThrow()
})

test("task without payloadSchema does not accept parsed payload", async () => {
  const noPayloadTask = task({
    id: "no-payload",
    sandbox: sb,
    run: async (ctx) => ctx.run.id,
  })

  await expect(parseTaskPayload(noPayloadTask, {})).rejects.toThrow('task "no-payload" does not accept payload')
})
