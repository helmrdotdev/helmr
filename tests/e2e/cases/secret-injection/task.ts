import { fixtureValue } from "../../support/runtime-mcp"
import { agent, computer, image } from "@helmr/sdk"
import { createHash } from "node:crypto"
import { z } from "zod"
const base = image("verification-secret").from("node:24-bookworm-slim")
export const secretComputer = computer({
  id: "helmr-secret-smoke", image: base, resources: { cpu: 1, memory: "1GiB" },
})
export const secretAgent = agent({
  id: "secret-smoke",
  computer: secretComputer,
  maxTurnDuration: "2m",
  turn: async (turn) => {
    const input = z.object({ sha256: z.string().length(64) }).strict().parse(fixtureValue(turn.input))
    const value = process.env.VERIFICATION_SECRET
    if (!value || createHash("sha256").update(value).digest("hex") !== input.sha256)
      throw new Error("Secret injection did not match")
    return { matched: true }
  },
})
