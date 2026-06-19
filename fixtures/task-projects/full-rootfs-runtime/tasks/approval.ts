import { task, wait } from "@helmr/sdk"

import { contractSandbox } from "../shared/sandboxes"
import { assert, assertVisibleFile } from "./_assert"
import { approvalDecision } from "./_waitpoint_schemas"

export const approval = task({
  id: "approval",
  sandbox: contractSandbox,
  maxDuration: 900,
  run: async (ctx) => {
    const token = await wait.createToken({
      timeout: 60,
      tags: ["fixture", "approval"],
      metadata: { prompt: "full-rootfs approval relay" },
    })
    const decision = await wait.forToken(token, {
      schema: approvalDecision,
      timeout: 60,
      tags: ["fixture", "approval"],
      metadata: { prompt: "full-rootfs approval relay" },
    }).unwrap()
    assert(decision.approved, "approval was denied")
    assert(decision.workspaceText !== undefined, "workspace text was not provided")
    await assertVisibleFile("/workspace/approval-workspace-write.txt", decision.workspaceText)
    return { runId: ctx.run.id, workspaceText: decision.workspaceText }
  },
})
