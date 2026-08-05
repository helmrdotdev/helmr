import { image, source, task, sandbox } from "@helmr/sdk"

const base = image("task-secrets")
  .from("node:24-bookworm-slim")
  .workdir("/sandbox")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .copy("/opt/helmr-task/package.json", source.file("package.json"))
  .workdir("/opt/helmr-task")
  .run(["bun", "install"])
  .workdir("/sandbox")

export const taskSecretsWorkspace = sandbox({ id: "task-secrets" })
  .image(base)
  .resources({ cpu: 1, memory: "1GiB" })

export const useSecret = task({
  id: "use-secret",
  maxDuration: "5m",
  run: async (ctx) => {
    if (!process.env.API_TOKEN) {
      throw new Error("API_TOKEN was not injected")
    }
    console.info({ secret: "API_TOKEN", available: true })
    return { ok: true }
  },
})
