import { fixtureValue } from "../../support/runtime-mcp"
import { agent, computer, image } from "@helmr/sdk"
import { readFile, writeFile } from "node:fs/promises"

export const verificationComputer = computer({
  id: "verification",
  image: image("verification").from("node:24-bookworm-slim").workdir("/sandbox"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const verificationAgent = agent({
  id: "verification-agent",
  computer: verificationComputer,
  maxTurnDuration: "2m",
  turn: async (turn, ctx) => {
    const input = fixtureValue(turn.input)
    if (input === null || typeof input !== "object" || Array.isArray(input) ||
        !("marker" in input) || typeof input["marker"] !== "string" || input["marker"].length === 0) {
      throw new Error("A nonempty marker is required")
    }
    await writeFile("verification-marker.txt", input["marker"])
    const observed = await readFile("verification-marker.txt", "utf8")
    if (observed !== input["marker"]) throw new Error("guest filesystem round trip failed")
    return { marker: observed, turnId: turn.id, sessionId: ctx.session.id, computerId: ctx.computer.id }
  },
})
