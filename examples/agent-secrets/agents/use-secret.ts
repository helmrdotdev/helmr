import { agent, template, image } from "@helmr/sdk"

const base = image("agent-secrets")
  .from("node:24-bookworm-slim")
  .workdir("/sandbox")

export const taskSecretsComputer = template({
  id: "agent-secrets",
  image: base,
  resources: { cpu: 1, memory: "1GiB" },
  // Create this Secret ID in each target Environment before deployment.
  secrets: [{ secret: "API_TOKEN", env: { name: "API_TOKEN", mode: "raw" } }],
})

export const useSecret = agent({
  template: taskSecretsComputer,
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
