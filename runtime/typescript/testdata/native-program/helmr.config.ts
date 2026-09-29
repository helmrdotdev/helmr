import { defineConfig } from "@helmr/sdk"
export default defineConfig({ dirs: ["tasks"], build: {
  external: ["sqlite3"],
  assets: ["mixed/asset.txt", "worker.mjs", "child.mjs"],
} })
