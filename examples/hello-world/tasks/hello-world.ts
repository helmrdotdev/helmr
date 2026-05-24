import { cache, image, sandbox, source, task } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"

const installNode24 = [
  "apt-get update",
  "apt-get install -y --no-install-recommends ca-certificates curl gnupg",
  "install -d -m 0755 /etc/apt/keyrings",
  "curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg",
  "echo 'deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_24.x nodistro main' > /etc/apt/sources.list.d/nodesource.list",
  "apt-get update",
  "apt-get install -y --no-install-recommends nodejs",
  "rm -rf /var/lib/apt/lists/*",
].join(" && ")

const base = image("hello-world")
  .from("oven/bun:1.3.10-debian")
  .workdir("/workspace")
  .run(["sh", "-ceu", installNode24])
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
