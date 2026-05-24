import { cache, image, sandbox, source, task } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"

const base = image("human-in-the-loop")
  .from("oven/bun:1.3.10-debian")
  .workdir("/workspace")
  .copy("/workspace/package.json", source.file("package.json"))
  .run(["bun", "install"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("human-in-the-loop-bun") }],
  })

const sbx = sandbox("human-in-the-loop")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi" })

export const handoff = task({
  id: "human-in-the-loop",
  sandbox: sbx,
  maxDuration: 900,
  run: async (_payload: unknown, ctx) => {
    const decision = await ctx.wait.approval("Continue and ask for a handoff note?")
    if (!decision.approved) {
      return { approved: false, approvedBy: decision.approvedBy }
    }

    const reply = await ctx.wait.message("What should this run write to handoff.txt?")
    await writeFile("handoff.txt", `${reply.text}\n`)
    return {
      approved: true,
      approvedBy: decision.approvedBy,
      messageFrom: reply.sentBy,
      path: "handoff.txt",
    }
  },
})
