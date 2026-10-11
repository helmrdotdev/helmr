import { defineConfig } from "@helmr/sdk"

export default defineConfig({
  dirs: ["./agents"],
  build: {
    external: ["@openai/codex", "@anthropic-ai/claude-agent-sdk", "@cursor/sdk"],
  },
})
