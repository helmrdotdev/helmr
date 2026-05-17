import { task } from "@helmr/sdk"

import { contractSandbox } from "../shared/sandboxes"

export const approvalMessage = task({
  id: "approval-message",
  sandbox: contractSandbox,
  maxDuration: 900,
  run: async (_payload, ctx) => {
    const decision = await ctx.wait.approval("continue?", { timeout: 60 })
    if (!decision.approved) {
      return { status: "denied" }
    }
    const reply = await ctx.wait.message("next instruction", { timeout: 60 })
    await Bun.write("/workspace/mixed-wait.txt", reply.text)
    return { runId: ctx.run.id, approvedBy: decision.approvedBy, text: reply.text }
  },
})
