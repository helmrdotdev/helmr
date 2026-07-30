import { image, source, task, workspace } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"
import { z } from "zod"

const base = image("hello-world")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .copy("/opt/helmr-task/package.json", source.file("package.json"))
  .workdir("/opt/helmr-task")
  .run(["bun", "install"])
  .workdir("/workspace")

export const helloWorldWorkspace = workspace("hello-world")
  .image(base)
  .resources({ cpu: 1, memory: "1GiB" })

const payload = z.object({
  name: z.string().optional(),
})

export const helloWorld = task({
  id: "hello-world",
  maxDuration: "5m",
  payload,
  run: async (payload, ctx) => {
    const name = payload.name?.trim() || "Helmr"
    const greeting = `hello ${name}`
    await writeFile("hello.txt", `${greeting}\nrun=${ctx.run.id}\n`)
    return { greeting, runId: ctx.run.id }
  },
})
