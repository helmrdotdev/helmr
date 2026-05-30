import { expect, test } from "bun:test"

import { parseTaskPayload } from "./internal"
import { PayloadSchemaValidationError, image, sandbox, task, type PayloadSchema } from "./index"

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

test("task parses payload through Standard Schema before run", async () => {
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

test("task accepts callable payload schemas with Standard Schema metadata", async () => {
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
