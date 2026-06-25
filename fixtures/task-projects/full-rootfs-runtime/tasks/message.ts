import { streams, task, tokens } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"

import { contractSandbox } from "../shared/sandboxes"
import { messageReply } from "./_token_schemas"

export const message = task({
  id: "message",
  sandbox: contractSandbox,
  maxDuration: 900,
  run: async (ctx) => {
    const token = await tokens.create({
      timeout: 60,
      tags: ["fixture", "message"],
      metadata: { prompt: "send workspace text" },
    })
    const reply = await tokens.wait(token, {
      schema: messageReply,
      timeout: 60,
      tags: ["fixture", "message"],
      metadata: { prompt: "send workspace text" },
    }).unwrap()
    await writeFile("/workspace/message-reply.txt", reply.text)
    await streams.output("agent.event").append({ content: [{ type: "text", text: reply.text }] })
    return { runId: ctx.run.id, text: reply.text }
  },
})
