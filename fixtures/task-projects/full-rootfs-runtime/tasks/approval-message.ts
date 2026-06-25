import { task, tokens } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"

import { contractSandbox } from "../shared/sandboxes"
import { approvalDecision, messageReply } from "./_token_schemas"

export const approvalMessage = task({
  id: "approval-message",
  sandbox: contractSandbox,
  maxDuration: 900,
  run: async (ctx) => {
    const decisionToken = await tokens.create({
      timeout: 60,
      tags: ["fixture", "approval"],
      metadata: { prompt: "continue?" },
    })
    const decision = await tokens.wait(decisionToken, {
      schema: approvalDecision,
      timeout: 60,
      tags: ["fixture", "approval"],
      metadata: { prompt: "continue?" },
    }).unwrap()
    if (!decision.approved) {
      return { status: "denied" }
    }
    const replyToken = await tokens.create({
      timeout: 60,
      tags: ["fixture", "message"],
      metadata: { prompt: "next instruction" },
    })
    const reply = await tokens.wait(replyToken, {
      schema: messageReply,
      timeout: 60,
      tags: ["fixture", "message"],
      metadata: { prompt: "next instruction" },
    }).unwrap()
    await writeFile("/workspace/mixed-wait.txt", reply.text)
    return { runId: ctx.run.id, text: reply.text }
  },
})
