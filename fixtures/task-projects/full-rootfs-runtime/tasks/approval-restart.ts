import { task } from "@helmr/sdk"

import { contractSandbox } from "../shared/sandboxes"
import { assert, assertVisibleFile } from "./_assert"

export const approvalRestart = task({
  id: "approval-restart",
  sandbox: contractSandbox,
  maxDuration: 900,
  run: async (_payload, ctx) => {
    const decision = await ctx.wait.approval("approval restart relay", { timeout: 60 })
    assert(decision.approved, "approval was denied")
    await new Promise((resolve) => setTimeout(resolve, 8_000))
    await assertVisibleFile("/workspace/approval-restart-workspace-write.txt", decision.approvedBy)
    console.log(JSON.stringify({ phase: "post-restart", approvedBy: decision.approvedBy }))
    return { runId: ctx.run.id, approvedBy: decision.approvedBy, phase: "post-restart" }
  },
})
