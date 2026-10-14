import { agent, type JsonValue } from "@helmr/sdk"
import { conversationComputer } from "../slack-conversation/agent"

export const setupFailure = agent({
  id: "setup-failure",
  template: conversationComputer,
  async init(): Promise<{ ready: boolean }> {
    throw new Error("Intentional initialization failure")
  },
  async turn(): Promise<JsonValue> {
    throw new Error("Initialization failure must prevent Turn execution")
  },
})
