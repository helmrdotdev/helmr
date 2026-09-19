import { task } from "@helmr/sdk"
import { readFileSync } from "node:fs"
import { createRequire } from "node:module"

// Declaration analysis imports this module inside the prepared environment.
const addon = createRequire(import.meta.url)("../build/Release/distro_addon.node")

// The value the install's lifecycle saw must be the one this later phase sees.
const installed = readFileSync(new URL("../generated/environment-stamp.txt", import.meta.url), "utf8")
const analyzed = readFileSync("/etc/helmr-environment-stamp", "utf8")
if (installed !== analyzed) {
  throw new Error(`install saw environment ${installed.trim()} but analysis sees ${analyzed.trim()}`)
}

export const native = task({ id: "native", run: () => JSON.parse(addon.run()) })
