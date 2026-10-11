import { fixtureValue } from "../../support/runtime-mcp"
import { setTimeout as sleep } from "node:timers/promises"
import { image, agent, computer } from "@helmr/sdk"
import { randomUUID } from "node:crypto"
import { readFile, writeFile } from "node:fs/promises"
import { z } from "zod"

export const outageComputer = computer({ id: "verification-outage", image: image("verification-outage").from("node:24-bookworm-slim").workdir("/workspace"), resources: { cpu: 1, memory: "1GiB" } })

export const outageAgent = agent({
  id: "verification-outage", computer: outageComputer, maxTurnDuration: "10m",
  turn: async (turn, ctx) => {
    const { marker } = z.object({ marker: z.string() }).strict().parse(fixtureValue(turn.input))
    const nonce = randomUUID()
    await writeFile("outage-marker", marker)
    await turn.output.write([{ type: "json", value: { phase: "started", marker, nonce } }])
    await sleep(360_000, undefined, { signal: turn.signal })
    if (await readFile("outage-marker", "utf8") !== marker) throw new Error("outage lost the file")
    return { marker, nonce, turnId: turn.id, sessionId: ctx.session.id }
  },
})
