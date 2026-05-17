import { task } from "@helmr/sdk"

import { contractSandbox } from "../shared/sandboxes"
import { assert, assertVisibleFile } from "./_assert"

export const approval = task({
  id: "approval",
  sandbox: contractSandbox,
  maxDuration: 900,
  run: async (_payload, ctx) => {
    const decision = await ctx.wait.approval("full-rootfs approval relay", { timeout: 60 })
    assert(decision.approved, "approval was denied")
    await assertVisibleFile("/workspace/approval-workspace-write.txt", decision.approvedBy)
    return { runId: ctx.run.id, approvedBy: decision.approvedBy }
  },
})
