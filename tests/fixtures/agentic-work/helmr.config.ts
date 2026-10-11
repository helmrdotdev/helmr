import { defineConfig } from "@helmr/sdk"

export default defineConfig({
  dirs: ["./agents"],
  build: {
    external: ["playwright", "sharp"],
    assets: ["probe/invoke.mjs", "work/tool.mjs", "work/analysis.py"],
  },
})
