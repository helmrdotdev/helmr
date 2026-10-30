import { agent, image, template } from "@helmr/sdk"
import { OpenCodeSession } from "./opencode-bridge"
import { agentName, introduction, model, smallModel, modelKey } from "./settings"

export const workspace = template({
  id: agentName,
  image: image(agentName)
    .from("node:24-bookworm-slim")
    .run(["sh", "-ceu", "apt-get update && apt-get install -y --no-install-recommends ca-certificates git ripgrep procps && rm -rf /var/lib/apt/lists/*"])
    .workdir("/workspace"),
  resources: { cpu: 2, memory: "4GiB" },
  deleteAfterUnused: "1m",
  secrets: [{ secret: modelKey.secret, env: { name: modelKey.env, mode: "protected", allowedOrigins: [modelKey.origin] } }],
})

export const assistant = agent({
  id: agentName, template: workspace, suspension: "manual", closeAfterIdle: "30m", maxTurnDuration: "15m",
  init({ session, recovery }) {
    return OpenCodeSession.open({ cwd: "/workspace", stateDirectory: "/var/lib/helmr/opencode", sessionId: session.id, model, introduction, recovery: recovery.kind,
      native: { config: { content: JSON.stringify({ agents: { title: { model: smallModel } } }) } },
    })
  },
  async turn(turn, { initResult }) {
    const { response, ...result } = await initResult.run(turn)
    if (response) await turn.respond(response)
    return result
  },
})
