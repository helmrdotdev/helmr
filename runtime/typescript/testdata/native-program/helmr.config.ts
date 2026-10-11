import { defineConfig } from "@helmr/sdk"
export default defineConfig({ dirs: ["agents"], build: {
  external: ["sqlite3"],
  assets: ["mixed/asset.txt", "worker.mjs", "child.mjs"],
} })
