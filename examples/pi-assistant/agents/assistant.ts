import { agent, image, source, template } from "@helmr/sdk"
import { PiSession, type PiOptions } from "./pi-bridge"
import { introduction, model } from "./settings"

export const workspace = template({
  id: "pi-analysis-workspace",
  image: image("pi-analysis-workspace")
    .from("node:24-bookworm-slim")
    .run(["sh", "-ceu", "apt-get update && apt-get install -y --no-install-recommends ca-certificates git ripgrep procps && rm -rf /var/lib/apt/lists/*"])
    .copy(source.directory("data"), "/workspace/data")
    .workdir("/workspace"),
  resources: { cpu: 2, memory: "4GiB" },
  secrets: [{ secret: "ANTHROPIC_API_KEY", env: { name: "ANTHROPIC_API_KEY", mode: "protected", allowedOrigins: ["https://api.anthropic.com"] } }],
})

export function createAnalysisAssistant(options: Partial<Pick<PiOptions, "cwd" | "stateDirectory" | "model" | "native">> = {}) {
  return agent({
    id: "pi-assistant", template: workspace, maxTurnDuration: "15m",
    init({ session, recovery }) {
      return PiSession.open({
        cwd: options.cwd ?? "/workspace", stateDirectory: options.stateDirectory ?? "/var/lib/helmr/pi",
        sessionId: session.id, recovery: recovery.kind, model: options.model ?? model,
        introduction, native: options.native,
      })
    },
    async turn(turn, { initResult }) {
      const { response, ...result } = await initResult.run(turn)
      if (response) await turn.respond(response)
      return result
    },
  })
}

export const assistant = createAnalysisAssistant()
