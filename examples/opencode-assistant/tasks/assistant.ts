import { normalizeInput } from "@helmr/sdk/internal"
import { agent, computer, image, MessageRejected } from "@helmr/sdk"
import { OpenCodeHarness } from "./opencode"

export const workspace = computer({
  id: "opencode-workspace",
  image: image("opencode-workspace")
    .from("node:24-bookworm-slim")
    .run(["sh", "-ceu", "apt-get update && apt-get install -y --no-install-recommends git ripgrep ca-certificates && rm -rf /var/lib/apt/lists/*"])
    .run(["npm", "install", "-g", "opencode-ai@1.18.30"])
    .workdir("/workspace"),
  resources: { cpu: 1, memory: "1GiB" },
  // Replace the example UUID with the existing Environment Secret ID.
  secrets: [{ secretId: "01900000-0000-7000-8000-000000000001", env: { name: "OPENCODE_GO_API_KEY", mode: "protected", allowedOrigins: ["https://opencode.ai"] } }],
})

export const assistant = agent({
  id: "opencode-assistant",
  computer: workspace,
  maxTurnDuration: "15m",
  async setup(context) {
    if (!process.env.OPENCODE_GO_API_KEY) throw new Error("Configure the OpenCode Go Environment Secret before starting")
    return OpenCodeHarness.open("/workspace", context.session.id, {
      model: "opencode-go/glm-5.3",
      small_model: "opencode-go/glm-5.3",
      enabled_providers: ["opencode-go"],
      provider: { "opencode-go": { options: { apiKey: "{env:OPENCODE_GO_API_KEY}" } } },
    })
  },
  async turn(turn, { setupResult }) {
    await turn.onMessage(async () => {
      throw new MessageRejected("Answer the current question, or enqueue a new instruction after this task")
    })
    return setupResult.run(turn, inputText(turn.input))
  },
})

export function inputText(value: unknown): string {
  const text = normalizeInput(value).map(part => part.text).join("")
  if (!text.trim()) throw new Error("Send a nonempty text message")
  return text
}
