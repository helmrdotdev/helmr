import { agent, computer, image } from "@helmr/sdk"
import { readFileSync } from "node:fs"
import { createRequire } from "node:module"

// Declaration analysis imports this module inside the prepared environment.
const addon = createRequire(import.meta.url)("#project/build/Release/distro_addon.node")

// The value the install's lifecycle saw must be the one this later phase sees.
const installed = readFileSync(new URL(import.meta.resolve("#project/generated/environment-stamp.txt")), "utf8")
const analyzed = readFileSync("/etc/helmr-environment-stamp", "utf8")
if (installed !== analyzed) {
  throw new Error(`install saw environment ${installed.trim()} but analysis sees ${analyzed.trim()}`)
}

export const native = agent({ id: "native", computer: computer({ id: "native-environment", image: image("native-environment").from("debian:trixie-slim@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132"), resources: { cpu: 1, memory: "1GiB" } }), turn: () => JSON.parse(addon.run()) })
