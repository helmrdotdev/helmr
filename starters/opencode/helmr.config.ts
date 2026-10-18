import { defineConfig } from "@helmr/sdk"

export default defineConfig({
  dirs: ["agents"],
  build: { external: ["@opencode/sdk", "@opencode/core", "@opencode/schema", "@opencode/util", "effect"] },
})
