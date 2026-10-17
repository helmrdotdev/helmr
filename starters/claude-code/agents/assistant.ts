import { agent, image, template } from "@helmr/sdk"
import { runClaude } from "./claude-bridge"
import { agentName, introduction, model, modelKey } from "./settings"

export const workspace = template({
  id: agentName,
  image: image(agentName)
    .from("node:24-bookworm-slim")
    .run(["node", "-e", "const fs = require('node:fs'); fs.mkdirSync('/etc/ssl/certs', { recursive: true }); fs.writeFileSync('/etc/ssl/certs/ca-certificates.crt', require('node:tls').rootCertificates.join(String.fromCharCode(10)) + String.fromCharCode(10))"])
    .run(["sh", "-ceu", "sed -i 's|http://deb.debian.org|https://deb.debian.org|g' /etc/apt/sources.list.d/debian.sources && apt-get update && apt-get install -y --no-install-recommends ca-certificates git ripgrep && rm -rf /var/lib/apt/lists/*"])
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
  async turn(turn, { session }) {
    const response = await runClaude(turn, { cwd: "/workspace", sessionId: session.id, model, appendSystemPrompt: introduction })
    if (response !== undefined) await turn.respond(response)
    return {}
  },
})
