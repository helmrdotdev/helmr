import { defineConfig } from "@helmr/sdk"

// Prepare an isolated project and explicitly select fixture directories for deployment.
export default defineConfig({ dirs: ["./cases"], ignorePatterns: ["**/run.ts", "**/*.test.*"] })
