import { task } from "@helmr/sdk"

import { contractSandbox } from "../shared/sandboxes"

export const message = task({
  id: "message",
  sandbox: contractSandbox,
  maxDuration: 900,
  run: async (_payload, ctx) => {
    const reply = await ctx.wait.message("send workspace text", { timeout: 60 })
    await Bun.write("/workspace/message-reply.txt", reply.text)
    ctx.emit({ type: "agent.event", content: [{ type: "text", text: reply.text }] })
    return { runId: ctx.run.id, text: reply.text, sentBy: reply.sentBy }
  },
})
