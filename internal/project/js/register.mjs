import { register } from "node:module"

const [major = 0, minor = 0] = process.versions.node.split(".").map((part) => Number.parseInt(part, 10))
if (major < 22 || (major === 22 && minor < 18)) {
  throw new Error(
    `Helmr TypeScript tasks require node >=22.18 in the sandbox image; found ${process.versions.node}`,
  )
}

register(new URL("./loader.mjs", import.meta.url))
