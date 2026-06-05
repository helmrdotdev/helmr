import { cache, image, sandbox, source, task } from "@helmr/sdk"

const base = image("task-secrets")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .copy("/workspace/package.json", source.file("package.json"))
  .run(["bun", "install"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("task-secrets-bun") }],
  })

const sbx = sandbox("task-secrets")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi" })

export const useSecret = task({
  id: "use-secret",
  sandbox: sbx,
  maxDuration: 300,
  secrets: [{ name: "API_TOKEN", env: "API_TOKEN" }],
  run: async (ctx) => {
    if (!process.env.API_TOKEN) {
      throw new Error("API_TOKEN was not injected")
    }
    ctx.log.info({ secret: "API_TOKEN", available: true })
    return { ok: true }
  },
})
