import { defineConfig } from "@helmr/sdk"

export default defineConfig({
  dirs: ["./tasks"],
  build: {
    external: ["playwright", "sharp"],
    assets: ["probe/invoke.mjs", "work/tool.mjs", "work/analysis.py"],
  },
})
