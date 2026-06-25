import { task, tokens } from "@helmr/sdk"

import { contractSandbox } from "../shared/sandboxes"
import { assert, assertVisibleFile } from "./_assert"
import { approvalDecision } from "./_token_schemas"

export const approvalRestart = task({
  id: "approval-restart",
  sandbox: contractSandbox,
  maxDuration: 900,
  run: async (ctx) => {
    const token = await tokens.create({
      timeout: 60,
      tags: ["fixture", "approval", "restart"],
      metadata: { prompt: "approval restart relay" },
    })
    const decision = await tokens.wait(token, {
      schema: approvalDecision,
      timeout: 60,
      tags: ["fixture", "approval", "restart"],
      metadata: { prompt: "approval restart relay" },
    }).unwrap()
    assert(decision.approved, "approval was denied")
    assert(decision.workspaceText !== undefined, "workspace text was not provided")
    await new Promise((resolve) => setTimeout(resolve, 8_000))
    await assertVisibleFile("/workspace/approval-restart-workspace-write.txt", decision.workspaceText)
    console.log(JSON.stringify({ phase: "post-restart", workspaceText: decision.workspaceText }))
    return { runId: ctx.run.id, workspaceText: decision.workspaceText, phase: "post-restart" }
  },
})
