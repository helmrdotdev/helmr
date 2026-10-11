import { agent, computer, image } from "@helmr/sdk"

// Analysis visits every declaration; running another declaration must not import
// this independent entry. This also makes the fixture exercise shared chunks.
if (!process.argv.includes("--analyze")) {
  throw new Error("unselected Program entry was imported during execution")
}

export const unselected = agent({ id: "unselected", computer: computer({ id: "unselected", image: image("unselected").from("debian:trixie-slim@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132"), resources: { cpu: 1, memory: "1GiB" } }), turn: () => "unused" })
