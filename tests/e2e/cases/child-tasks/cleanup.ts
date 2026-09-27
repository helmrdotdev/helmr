import type { HelmrClient, Run } from "@helmr/sdk"
import { deadline, errorCode, waitRun } from "../../support/context"

// A failed parent may never return its child's ID. Discover only this parent's
// children in its two owned Workspaces before the Workspaces are deleted.
export async function cleanupChildren(
  client: HelmrClient,
  parentId: string,
  workspaceIds: readonly string[],
  recorded: string[],
) {
  const signal = deadline(30_000)
  const children: Run[] = []
  let cursor: string | undefined
  do {
    const page = await client.runs.list({ kind: "task", limit: 100, cursor }, { signal })
    for (const item of page.items) {
      if (!workspaceIds.includes(item.workspaceId) || item.id === parentId) continue
      const run = await client.runs.retrieve(item.id, { signal })
      if (run.parentRunId !== parentId) continue
      if (!recorded.includes(run.id)) recorded.push(run.id)
      children.push(run)
    }
    cursor = page.nextCursor
  } while (cursor !== undefined)
  // Cancellation can take longer than discovery's request budget.
  for (const run of children) {
    if (!["succeeded", "failed", "system_failed", "cancelled", "expired"].includes(run.status)) {
      try {
        await client.runs.cancel(run.id, {}, { signal: deadline(30_000) })
      } catch (error) {
        // A child can finish after discovery. Prove terminal state below.
        if (errorCode(error) !== "run_lifecycle_conflict") throw error
      }
      await waitRun(client, run.id, ["succeeded", "failed", "system_failed", "cancelled", "expired"], 120_000)
    }
  }
}
