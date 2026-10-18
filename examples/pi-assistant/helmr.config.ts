import { defineConfig } from "@helmr/sdk"

export default defineConfig({
  dirs: ["agents"],
  build: { external: ["@earendil-works/pi-coding-agent"] },
})
