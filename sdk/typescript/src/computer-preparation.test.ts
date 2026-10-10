import { expect, test } from "bun:test"
import { createClientComputers } from "./client-computer"

const computer = {
  id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
  definition_key: "repository-agent",
  deployment_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35",
  status: "available",
      residency: "unavailable",
  secrets: [],
  error: { code: "computer_preparation_exhausted", message: "Computer preparation limit reached" },
  last_activity_at: "2026-07-24T11:50:00Z",
  created_at: "2026-07-24T11:50:00Z",
  updated_at: "2026-07-24T11:50:00Z",
}

test("Computer preparation failure remains readable through retrieve and list", async () => {
  const client = createClientComputers({
    async request(_method, path) {
      return path.startsWith("/v1/computers/") ? computer : { computers: [computer] }
    },
  })
  const retrieved = await client.retrieve(computer.id)
  const listed = await client.list()
  expect(retrieved.error?.code).toBe("computer_preparation_exhausted")
  expect(listed.items[0]?.error).toEqual(retrieved.error)
  expect(Object.isFrozen(retrieved.error)).toBe(true)
  expect(retrieved.id).toBe(computer.id)
})
