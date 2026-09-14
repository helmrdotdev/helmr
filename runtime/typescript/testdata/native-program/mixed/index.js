import { createRequire } from "node:module"
export { state } from "@state"
const require = createRequire(import.meta.url)
export const again = async () => {
  const value = await import("./directory")
  if (value.state !== require("./state#%?").state) throw new Error("require module identity changed")
  return value
}
