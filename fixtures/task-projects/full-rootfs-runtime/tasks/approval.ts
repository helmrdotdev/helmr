import { task } from "@helmr/sdk"

import { contractSandbox } from "../shared/sandboxes"
import { assert, assertVisibleFile } from "./_assert"

export const approval = task({
  id: "approval",
  sandbox: contractSandbox,
  maxDuration: 900,
  run: async (ctx) => {
    const decision = await ctx.wait.token<{ approved: boolean; workspaceText: string }>({
      displayText: "full-rootfs approval relay",
      timeout: 60,
    })
    assert(decision.approved, "approval was denied")
    await assertVisibleFile("/workspace/approval-workspace-write.txt", decision.workspaceText)
    return { runId: ctx.run.id, workspaceText: decision.workspaceText }
  },
})
