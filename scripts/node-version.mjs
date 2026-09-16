import { readFileSync } from "node:fs"

export const nodeVersion = JSON.parse(readFileSync(new URL("../internal/version/runtime-dependencies.json", import.meta.url), "utf8")).node.version
if (!/^\d+\.\d+\.\d+$/.test(nodeVersion)) throw new Error("invalid Product Node version")
