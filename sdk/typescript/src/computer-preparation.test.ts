import { expect, test } from "bun:test"
import { createClientComputers } from "./client-computer"

const computer = {
  id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
  template: "repository-agent",
      delete_after_unused_ms: null, unused_since: null, execution_wait: null,
  deployment_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
  status: "available",
      residency: "unavailable",
  secrets: [],
  error: { code: "computer_preparation_exhausted", message: "Computer preparation limit reached" },
  last_activity_at: "2026-07-24T11:50:00Z",
  created_at: "2026-07-24T11:50:00Z",
  updated_at: "2026-07-24T11:50:00Z",
}

test("Computer preparation failure remains readable through get and list", async () => {
  const client = createClientComputers({
    async request(_method, path) {
      return path.startsWith("/v1/computers/") ? computer : { computers: [computer] }
    },
  })
  const retrieved = await client.get(computer.id)
  const listed = await client.list()
  expect(retrieved.error?.code).toBe("computer_preparation_exhausted")
  expect(listed.items[0]?.error).toEqual(retrieved.error)
  expect(Object.isFrozen(retrieved.error)).toBe(true)
  expect(retrieved.id).toBe(computer.id)
})

for (const reason of ["scheduling", "environment_limit", "saving"] as const) {
  test(`Computer execution wait ${reason} is exposed through get and list`, async () => {
    const response = { ...computer, residency: "cold", execution_wait: { reason } }
    const client = createClientComputers({
      async request(_method, path) {
        return path.startsWith("/v1/computers/") ? response : { computers: [response] }
      },
    })
    expect((await client.get(computer.id)).executionWait).toEqual({ reason })
    expect((await client.list()).items[0]?.executionWait).toEqual({ reason })
  })
}

for (const value of [undefined, {}, { reason: "warm" }, "saving", []]) {
  test(`Computer rejects malformed execution wait ${JSON.stringify(value)}`, async () => {
    const client = createClientComputers({ async request() { return { ...computer, execution_wait: value } } })
    await expect(client.get(computer.id)).rejects.toThrow("execution_wait")
  })
}

test("Computer list by Secret reports how each Computer holds the raw value", async () => {
  const paths: string[] = []
  const client = createClientComputers({
    async request(_method, path) {
      paths.push(path)
      return { computers: [
        { ...computer, id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33", secret_exposure: "received" },
        { ...computer, secret_exposure: "image" },
      ] }
    },
  })
  const listed = await client.list({ secret: "DATABASE_PASSWORD", limit: 10 })
  expect(paths).toEqual(["/v1/computers?secret=DATABASE_PASSWORD&limit=10"])
  expect(listed.items.map((item) => item.secretExposure)).toEqual(["received", "image"])
  expect((await client.list()).items[0]?.secretExposure).toBe("received")
  await expect(client.list({ secret: "" })).rejects.toThrow("secret")
  await expect(client.list({ key: "repository", secret: "TOKEN" } as never)).rejects.toThrow("secret")
  const invalid = createClientComputers({ async request() { return { computers: [{ ...computer, secret_exposure: "copied" }] } } })
  await expect(invalid.list({ secret: "TOKEN" })).rejects.toThrow("secret_exposure")
})
