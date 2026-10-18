import { agent, image, template } from "@helmr/sdk"
import { PiSession } from "./pi-bridge"
import { agentName, introduction, model, modelKey } from "./settings"

export const workspace = template({
  id: agentName,
  image: image(agentName)
    .from("node:24-bookworm-slim")
    .run(["node", "-e", "const fs = require('node:fs'); fs.mkdirSync('/etc/ssl/certs', { recursive: true }); fs.writeFileSync('/etc/ssl/certs/ca-certificates.crt', require('node:tls').rootCertificates.join(String.fromCharCode(10)) + String.fromCharCode(10))"])
    .run(["sh", "-ceu", "sed -i 's|http://deb.debian.org|https://deb.debian.org|g' /etc/apt/sources.list.d/debian.sources && apt-get update && apt-get install -y --no-install-recommends ca-certificates git ripgrep procps && rm -rf /var/lib/apt/lists/*"])
    .workdir("/workspace"),
  resources: { cpu: 2, memory: "4GiB" },
  secrets: [{ secret: modelKey.secret, env: { name: modelKey.env, mode: "protected", allowedOrigins: [modelKey.origin] } }],
})

export const assistant = agent({
  id: agentName, template: workspace, maxTurnDuration: "15m",
  init({ session, recovery }) {
    return PiSession.open({ cwd: "/workspace", stateDirectory: "/var/lib/helmr/pi", sessionId: session.id, model, introduction, recovery: recovery.kind })
  },
  async turn(turn, { initResult }) {
    const { response, ...result } = await initResult.run(turn)
    if (response) await turn.respond(response)
    return result
  },
})
