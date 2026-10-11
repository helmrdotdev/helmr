import { agent, computer, image, source } from "@helmr/sdk"

const base = image("task-secrets")
  .from("node:24-bookworm-slim")
  .workdir("/sandbox")
  .run(["npm", "install", "-g", "bun@1.3.13"])
  .copy(source.file("package.json"), "/opt/helmr-task/package.json")
  .workdir("/opt/helmr-task")
  .run(["bun", "install"])
  .workdir("/sandbox")

export const taskSecretsComputer = computer({
  id: "task-secrets",
  image: base,
  resources: { cpu: 1, memory: "1GiB" },
  // Replace this example UUID with the Environment Secret ID before deployment.
  secrets: [{ secretId: "01900000-0000-7000-8000-000000000001", env: { name: "API_TOKEN", mode: "raw" } }],
})

export const useSecret = agent({
  computer: taskSecretsComputer,
  id: "use-secret",
  maxTurnDuration: "5m",
  async turn() {
    if (!process.env["API_TOKEN"]) {
      throw new Error("API_TOKEN was not injected")
    }
    console.info({ secret: "API_TOKEN", available: true })
    return { ok: true }
  },
})
