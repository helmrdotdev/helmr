import { cache, image, logger, sandbox, source, task } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"
import { z } from "zod"

const base = image("hello-world")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .copy("/opt/helmr-task/package.json", source.file("package.json"))
  .workdir("/opt/helmr-task")
  .run(["bun", "install"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("hello-world-bun") }],
  })
  .workdir("/workspace")

const sbx = sandbox("hello-world")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi" })

const payload = z.object({
  name: z.string().optional(),
})

export const helloWorld = task({
  id: "hello-world",
  sandbox: sbx,
  maxDuration: 300,
  payload,
  run: async (payload, ctx) => {
    const name = payload.name?.trim() || "Helmr"
    const greeting = `hello ${name}`
    await writeFile("hello.txt", `${greeting}\nrun=${ctx.run.id}\n`)
    logger.info({ message: "wrote greeting", path: "hello.txt" })
    return { greeting, runId: ctx.run.id }
  },
})
