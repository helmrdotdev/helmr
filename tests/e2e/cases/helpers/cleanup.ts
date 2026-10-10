import type { HelmrClient } from "@helmr/sdk"
import { deadline } from "../../support/context"

// A failed caller may never return its helper's ID. Discover both owned and
// independently requested Sessions, restricted to this case's Computers.
export async function cleanupChildren(
  client: HelmrClient, parentId: string, computerIds: readonly string[], recorded: string[],
) {
  const signal = deadline(30_000)
  const ids = new Set<string>()
  for (const relation of ["parentSessionId", "requesterSessionId"] as const) {
    let cursor: string | undefined
    do {
      const page = await client.sessions.list({ [relation]: parentId, limit: 100, cursor }, { signal })
      for (const item of page.items) {
        if (item.id === parentId || item[relation] !== parentId || !computerIds.includes(item.computerId)) continue
        ids.add(item.id)
        if (!recorded.includes(item.id)) recorded.push(item.id)
      }
      cursor = page.nextCursor
    } while (cursor !== undefined)
  }
  const failures: unknown[] = []
  for (const id of ids) {
    try {
      await client.sessions.get(id).cancel({ idempotencyKey: `helper-cleanup:${parentId}:${id}` }, { signal: deadline(30_000) })
    } catch (error) { failures.push(error) }
  }
  if (failures.length) throw new AggregateError(failures, "Helper Session cleanup failed")
}
