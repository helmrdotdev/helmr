import { readFileSync } from "node:fs"
export const state = { asset: readFileSync(new URL(import.meta.resolve("#project/mixed/asset.txt")), "utf8").trim() }
