import { cache, image, sandbox, source, task } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"

const base = image("human-in-the-loop")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .run(["npm", "install", "-g", "bun@1.3.10"])
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
  run: async (ctx) => {
    const decision = await ctx.wait.token<{ approved: boolean }>({
      displayText: "Continue and ask for a handoff note?",
    })
    if (!decision.approved) {
      return { approved: false }
    }

    const reply = await ctx.wait.token<{ text: string }>({
      displayText: "What should this run write to handoff.txt?",
    })
    await writeFile("handoff.txt", `${reply.text}\n`)
    return {
      approved: true,
      path: "handoff.txt",
    }
  },
})
