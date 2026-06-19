import { task, wait } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"

import { contractSandbox } from "../shared/sandboxes"
import { approvalDecision, messageReply } from "./_waitpoint_schemas"

export const approvalMessage = task({
  id: "approval-message",
  sandbox: contractSandbox,
  maxDuration: 900,
  run: async (ctx) => {
    const decisionToken = await wait.createToken({
      timeout: 60,
      tags: ["fixture", "approval"],
      metadata: { prompt: "continue?" },
    })
    const decision = await wait.forToken(decisionToken, {
      schema: approvalDecision,
      timeout: 60,
      tags: ["fixture", "approval"],
      metadata: { prompt: "continue?" },
    }).unwrap()
    if (!decision.approved) {
      return { status: "denied" }
    }
    const replyToken = await wait.createToken({
      timeout: 60,
      tags: ["fixture", "message"],
      metadata: { prompt: "next instruction" },
    })
    const reply = await wait.forToken(replyToken, {
      schema: messageReply,
      timeout: 60,
      tags: ["fixture", "message"],
      metadata: { prompt: "next instruction" },
    }).unwrap()
    await writeFile("/workspace/mixed-wait.txt", reply.text)
    return { runId: ctx.run.id, text: reply.text }
  },
})
