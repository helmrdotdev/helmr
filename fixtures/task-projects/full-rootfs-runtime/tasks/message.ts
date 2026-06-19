import { task, wait } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"

import { contractSandbox } from "../shared/sandboxes"
import { messageReply } from "./_waitpoint_schemas"

export const message = task({
  id: "message",
  sandbox: contractSandbox,
  maxDuration: 900,
  run: async (ctx) => {
    const token = await wait.createToken({
      timeout: 60,
      tags: ["fixture", "message"],
      metadata: { prompt: "send workspace text" },
    })
    const reply = await wait.forToken(token, {
      schema: messageReply,
      timeout: 60,
      tags: ["fixture", "message"],
      metadata: { prompt: "send workspace text" },
    }).unwrap()
    await writeFile("/workspace/message-reply.txt", reply.text)
    await ctx.session.output("agent.event").append({ content: [{ type: "text", text: reply.text }] })
    return { runId: ctx.run.id, text: reply.text }
  },
})
