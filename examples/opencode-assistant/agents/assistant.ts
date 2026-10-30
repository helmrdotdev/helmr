import { agent, image, template } from "@helmr/sdk"
import { join } from "node:path"
import { OpenCodeSession, type OpenCodeOptions } from "./opencode-bridge"
import { introduction, model, nativeOptions } from "./settings"

export const workspace = template({
  id: "opencode-workspace",
  image: image("opencode-workspace")
    .from("node:24-bookworm-slim")
    .run(["sh", "-ceu", "apt-get update && apt-get install -y --no-install-recommends ca-certificates git ripgrep procps && rm -rf /var/lib/apt/lists/*"])
    .workdir("/workspace"),
  resources: { cpu: 2, memory: "4GiB" },
  secrets: [{ secret: "OPENCODE_GO_API_KEY", env: { name: "OPENCODE_API_KEY", mode: "protected", allowedOrigins: ["https://opencode.ai"] } }],
})

// Keep paths and native configuration explicit so this application can also be
// exercised locally with a temporary workspace and a synthetic model.
export function createWebsiteAssistant(options: Partial<Pick<OpenCodeOptions, "cwd" | "stateDirectory" | "model" | "native">> = {}) {
  const stateDirectory = options.stateDirectory ?? "/var/lib/helmr/opencode"
  return agent({
    id: "opencode-assistant", template: workspace, maxTurnDuration: "15m",
    init({ session, recovery }) {
      return OpenCodeSession.open({
        cwd: options.cwd ?? "/workspace", stateDirectory, sessionId: session.id,
        model: options.model ?? model, introduction, recovery: recovery.kind,
        native: options.native ?? nativeOptions(join(stateDirectory, encodeURIComponent(session.id), "config")),
      })
    },
    async turn(turn, { initResult }) {
      const { response, ...result } = await initResult.run(turn)
      if (response) await turn.respond(response)
      return result
    },
  })
}

export const assistant = createWebsiteAssistant()
