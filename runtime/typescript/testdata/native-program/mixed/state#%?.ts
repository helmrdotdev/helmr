import { readFileSync } from "node:fs"
export const state = { asset: readFileSync(new URL("./asset.txt", import.meta.url), "utf8").trim() }
