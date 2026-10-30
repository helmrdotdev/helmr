import { agent, image, template } from "@helmr/sdk"
import { PiSession } from "./pi-bridge"
import { agentName, introduction, model, modelKey } from "./settings"

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
    return PiSession.open({ cwd: "/workspace", stateDirectory: "/var/lib/helmr/pi", sessionId: session.id, model, introduction, recovery: recovery.kind })
  },
  async turn(turn, { initResult }) {
    const { response, ...result } = await initResult.run(turn)
    if (response) await turn.respond(response)
    return result
  },
})
