import { agent, image, template } from "@helmr/sdk"
import { ClaudeSession } from "./claude-bridge"
import { agentName, introduction, model, modelKey } from "./settings"

export const workspace = template({
  id: agentName,
  image: image(agentName)
    .from("node:24-bookworm-slim")
    .run(["sh", "-ceu", "apt-get update && apt-get install -y --no-install-recommends ca-certificates git ripgrep procps && rm -rf /var/lib/apt/lists/*"])
    .workdir("/workspace"),
  resources: { cpu: 2, memory: "4GiB" },
  // Add this Secret in the Console for each Environment you deploy to. The key
  // is only sent to the model provider; the Computer sees a placeholder.
  secrets: [{ secret: modelKey.secret, env: { name: modelKey.env, mode: "protected", allowedOrigins: [modelKey.origin] } }],
})

export const assistant = agent({
  id: agentName,
  template: workspace,
  maxTurnDuration: "15m",
  init({ session, recovery }) {
    return new ClaudeSession({ cwd: "/workspace", stateDirectory: "/var/lib/helmr/claude", sessionId: session.id, model, appendSystemPrompt: introduction, recovery: recovery.kind })
  },
  async turn(turn, { initResult }) {
    const { response, ...result } = await initResult.run(turn)
    if (response !== undefined) await turn.respond(response)
    return result
  },
})
