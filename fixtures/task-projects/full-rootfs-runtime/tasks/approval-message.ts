import { task } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"

import { contractSandbox } from "../shared/sandboxes"

export const approvalMessage = task({
  id: "approval-message",
  sandbox: contractSandbox,
  maxDuration: 900,
  run: async (ctx) => {
    const decision = await ctx.wait.manual<{ approved: boolean }>({
      displayText: "continue?",
      timeout: 60,
    })
    if (!decision.approved) {
      return { status: "denied" }
    }
    const reply = await ctx.wait.manual<{ text: string }>({
      displayText: "next instruction",
      timeout: 60,
    })
    await writeFile("/workspace/mixed-wait.txt", reply.text)
    return { runId: ctx.run.id, text: reply.text }
  },
})
