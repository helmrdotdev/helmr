import { cache, image, sandbox, source, task } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"

const base = image("hello-world")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .copy("/workspace/package.json", source.file("package.json"))
  .run(["bun", "install"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("hello-world-bun") }],
  })

const sbx = sandbox("hello-world")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi" })

interface Payload {
  readonly name?: string
}

export const helloWorld = task({
  id: "hello-world",
  sandbox: sbx,
  maxDuration: 300,
  run: async (payload: Payload, ctx) => {
    const name = payload.name?.trim() || "Helmr"
    const greeting = `hello ${name}`
    await writeFile("hello.txt", `${greeting}\nrun=${ctx.run.id}\n`)
    ctx.log.info({ message: "wrote greeting", path: "hello.txt" })
    return { greeting, runId: ctx.run.id }
  },
})
