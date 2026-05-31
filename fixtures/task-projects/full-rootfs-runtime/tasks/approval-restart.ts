import { task } from "@helmr/sdk"

import { contractSandbox } from "../shared/sandboxes"
import { assert, assertVisibleFile } from "./_assert"

export const approvalRestart = task({
  id: "approval-restart",
  sandbox: contractSandbox,
  maxDuration: 900,
  run: async (ctx) => {
    const decision = await ctx.wait.manual<{ approved: boolean; workspaceText: string }>({
      displayText: "approval restart relay",
      timeout: 60,
    })
    assert(decision.approved, "approval was denied")
    await new Promise((resolve) => setTimeout(resolve, 8_000))
    await assertVisibleFile("/workspace/approval-restart-workspace-write.txt", decision.workspaceText)
    console.log(JSON.stringify({ phase: "post-restart", workspaceText: decision.workspaceText }))
    return { runId: ctx.run.id, workspaceText: decision.workspaceText, phase: "post-restart" }
  },
})
