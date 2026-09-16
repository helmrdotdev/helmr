import { readFileSync } from "node:fs"

export const nodeVersion = JSON.parse(readFileSync(new URL("../internal/version/node-release.json", import.meta.url), "utf8")).version
if (!/^\d+\.\d+\.\d+$/.test(nodeVersion)) throw new Error("invalid Product Node version")
