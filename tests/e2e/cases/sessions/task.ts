import { fixtureValue } from "../../support/runtime-mcp"
import { agent } from "@helmr/sdk"
import { randomUUID } from "node:crypto"
import { readFile, writeFile } from "node:fs/promises"
import { z } from "zod"
import { verificationComputer } from "../agent/task"

export const verificationSession = agent({
  id: "verification-session", computer: verificationComputer, maxTurnDuration: "10m",
  setup: () => ({ nonce: randomUUID(), count: 0, marker: "" }),
  turn: async (turn, ctx) => {
    const input = z.object({ marker: z.string().uuid() }).strict().parse(fixtureValue(turn.input))
    const memory = ctx.setupResult
    const file = `/root/verification-session-${memory.nonce}.json`
    memory.count += 1
    if (memory.count === 1) {
      memory.marker = input.marker
      await writeFile(file, JSON.stringify({ marker: memory.marker, nonce: memory.nonce }), { flag: "wx" })
    } else if (memory.count !== 2 || input.marker !== memory.marker) {
      throw new Error("Session memory or Turn sequence changed")
    }
    const stored = JSON.parse(await readFile(file, "utf8"))
    if (stored.marker !== memory.marker || stored.nonce !== memory.nonce) throw new Error("Session filesystem state changed")
    const result = { marker: memory.marker, nonce: memory.nonce, count: memory.count, sessionId: ctx.session.id, computerId: ctx.computer.id }
    if (memory.count === 2) {
      let finish!: () => void
      const observed = new Promise<void>(resolve => { finish = resolve })
      await turn.onMessage(message => { if (fixtureValue(message) === "finish") finish() })
      await turn.output.write([{ type: "json", value: { ...result, phase: "restored" } }])
      await observed
    }
    return result
  },
})
