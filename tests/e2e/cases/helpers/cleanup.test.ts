import { test, expect } from "bun:test"
import type { HelmrClient } from "@helmr/sdk"
import { cleanupChildren } from "./cleanup"

for (const cancelFails of [false, true]) {
  test(`helper cleanup paginates both relationships and preserves other owners (failure=${cancelFails})`, async () => {
    const queries: unknown[] = [], cancelled: string[] = []
    const client = { sessions: {
      list: async (query: { cursor?: string; parentSessionId?: string; requesterSessionId?: string }) => {
        queries.push(query)
        const relation = query.parentSessionId ? "parentSessionId" : "requesterSessionId"
        return query.cursor === undefined ? {
          items: [
            { id: "owned", computerId: "ours", [relation]: "parent" },
            { id: "other-computer", computerId: "theirs", [relation]: "parent" },
            { id: "other-owner", computerId: "ours", [relation]: "other" },
          ], nextCursor: "next",
        } : { items: [{ id: relation === "parentSessionId" ? "owned" : "independent", computerId: "ours", [relation]: "parent" }] }
      },
      get: (id: string) => ({ cancel: async () => {
        expect(queries).toHaveLength(4)
        cancelled.push(id)
        if (cancelFails && id === "owned") throw new Error("cancel failed")
      } }),
    } } as unknown as HelmrClient
    const recorded = ["parent", "owned"]
    const result = cleanupChildren(client, "parent", ["ours"], recorded)
    if (cancelFails) await expect(result).rejects.toThrow("Helper Session cleanup failed")
    else await result
    expect(cancelled).toEqual(["owned", "independent"])
    expect(recorded).toEqual(["parent", "owned", "independent"])
    expect(queries).toEqual([
      { parentSessionId: "parent", limit: 100, cursor: undefined },
      { parentSessionId: "parent", limit: 100, cursor: "next" },
      { requesterSessionId: "parent", limit: 100, cursor: undefined },
      { requesterSessionId: "parent", limit: 100, cursor: "next" },
    ])
  })
}
