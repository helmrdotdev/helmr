import type { HelmrClient } from "@helmr/sdk"
import { deadline, waitRun } from "../../support/context"

// A failed parent may never return its child's ID. Discover only this parent's
// children in its two owned Workspaces before the Workspaces are deleted.
export async function cleanupChildren(
  client: HelmrClient,
  parentId: string,
  workspaceIds: readonly string[],
  recorded: string[],
) {
  const signal = deadline(30_000)
  let cursor: string | undefined
  do {
    const page = await client.runs.list({ kind: "task", limit: 100, cursor }, { signal })
    for (const item of page.items) {
      if (!workspaceIds.includes(item.workspaceId) || item.id === parentId) continue
      const run = await client.runs.retrieve(item.id, { signal })
      if (run.parentRunId !== parentId) continue
      if (!recorded.includes(run.id)) recorded.push(run.id)
      if (!["succeeded", "failed", "system_failed", "cancelled", "expired"].includes(run.status)) {
        await client.runs.cancel(run.id, {}, { signal: deadline(30_000) })
        await waitRun(client, run.id, ["succeeded", "failed", "system_failed", "cancelled", "expired"], 120_000)
      }
    }
    cursor = page.nextCursor
  } while (cursor !== undefined)
}
