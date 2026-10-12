import { describe, expect, test } from "bun:test"
import { HelmrClient } from "./client"

const agentId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc39"
const deploymentId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
describe("Agent definition inspection", () => {
  test("lists and retrieves pinned definitions with cancellation", async () => {
    const requests: Array<{ url: string; signal: AbortSignal | null | undefined }> = []
    const client = new HelmrClient({ apiKey: "key", url: "https://api.example.test", fetch: (async (input, init) => {
      requests.push({ url: String(input), signal: init?.signal })
      return Response.json(requests.length === 1
        ? { deployment_id: deploymentId, agents: [{ agent: "worker", template: "machine", agent_id: agentId }], next_cursor: "next" }
        : { deployment_id: deploymentId, agent: "worker", template: "machine", agent_id: agentId })
    }) as typeof fetch })
    const signal = new AbortController().signal
    expect(await client.agents.list({ deploymentId, cursor: "after", limit: 2 }, { signal })).toEqual({ deploymentId, items: [{ agent: "worker", template: "machine", agentId }], nextCursor: "next" })
    expect(await client.agents.get("worker", { deploymentId }, { signal })).toEqual({ deploymentId, agent: "worker", template: "machine", agentId })
    expect(requests).toEqual([
      { url: `https://api.example.test/v1/agents?deployment_id=${deploymentId}&cursor=after&limit=2`, signal },
      { url: `https://api.example.test/v1/agents/worker?deployment_id=${deploymentId}`, signal },
    ])
  })
  test("rejects malformed responses and invalid queries", async () => {
    let response: unknown = {}
    let calls = 0
    const client = new HelmrClient({ apiKey: "key", fetch: (async () => { calls++; return Response.json(response) }) as typeof fetch })
    for (response of [null, [], { agents: [] }, { deployment_id: deploymentId, agents: [{}] }, { deployment_id: deploymentId, agents: [], next_cursor: 4 }]) {
      await expect(client.agents.list()).rejects.toThrow()
    }
    for (response of [null, { deployment_id: deploymentId, agent: "worker", template: "machine" }, { deployment_id: deploymentId, agent: "worker", template: "machine", agent_id: "worker" }, { agent: "worker", template: "machine", agent_id: agentId }, { deployment_id: deploymentId, agent: "bad/id" }]) {
      await expect(client.agents.get("worker")).rejects.toThrow()
    }
    const before = calls
    await expect(client.agents.list({ limit: 0 })).rejects.toThrow()
    await expect(client.agents.list({ cursor: "" })).rejects.toThrow()
    await expect(client.agents.get("bad/id")).rejects.toThrow()
    await expect(client.agents.get("worker", { cursor: "unexpected" } as never)).rejects.toThrow("item query")
    expect(calls).toBe(before)
    response = { deployment_id: deploymentId, agents: [] }
    expect(await client.agents.list()).toEqual({ deploymentId, items: [] })
  })
})

// Authored IDs and Agent UUIDs remain different even when both look like UUIDs.
test("a UUID-shaped definition ID is never interpreted as the Agent UUID", async () => {
  const definitionId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc40"
  const client = new HelmrClient({ apiKey: "key", fetch: (async (input) => {
    expect(new URL(String(input)).pathname).toBe(`/v1/agents/${definitionId}`)
    return Response.json({ agent: definitionId, template: "machine", agent_id: agentId, deployment_id: deploymentId })
  }) as typeof fetch })
  expect(await client.agents.get(definitionId)).toEqual({ agent: definitionId, template: "machine", agentId, deploymentId })
})
