import { fixtureValue } from "../../support/runtime-mcp"
import { setTimeout as sleep } from "node:timers/promises"
import { image, agent, computer } from "@helmr/sdk"
import { randomUUID } from "node:crypto"
import { readFile, writeFile } from "node:fs/promises"
import { z } from "zod"

export const renewalComputer = computer({ id: "verification-drain-renewal", image: image("verification-drain-renewal").from("node:24-bookworm-slim").workdir("/workspace"), resources: { cpu: 1, memory: "1GiB" } })

export const renewalAgent = agent({
  id: "verification-drain-renewal", computer: renewalComputer, maxTurnDuration: "40m",
  turn: async (turn, ctx) => {
    const { marker } = z.object({ marker: z.string() }).strict().parse(fixtureValue(turn.input))
    const nonce = randomUUID()
    await writeFile("drain-renewal-marker", marker)
    await turn.output.write([{ type: "json", value: { phase: "started", marker, nonce } }])
    // Ordinary JS timers keep this Turn active throughout planned drain,
    // exercising continued execution grants before the Turn completes.
    for (let tick = 1; tick <= 72; tick++) {
      await sleep(30_000, undefined, { signal: turn.signal })
      await turn.output.write([{ type: "json", value: { phase: "running", marker, nonce, tick } }])
    }
    if (await readFile("drain-renewal-marker", "utf8") !== marker) throw new Error("drain lost the file")
    return { marker, nonce, turnId: turn.id, sessionId: ctx.session.id }
  },
})
