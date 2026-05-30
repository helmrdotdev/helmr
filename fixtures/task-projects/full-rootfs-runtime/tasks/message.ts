import { task } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"

import { contractSandbox } from "../shared/sandboxes"

export const message = task({
  id: "message",
  sandbox: contractSandbox,
  maxDuration: 900,
  run: async (ctx) => {
    const reply = await ctx.wait.token<{ text: string }>({
      displayText: "send workspace text",
      timeout: 60,
    })
    await writeFile("/workspace/message-reply.txt", reply.text)
    ctx.emit({ type: "agent.event", content: [{ type: "text", text: reply.text }] })
    return { runId: ctx.run.id, text: reply.text }
  },
})
