import { fixtureValue } from "../../support/runtime-mcp"
import { agent } from "@helmr/sdk"
import { randomUUID } from "node:crypto"
import { readFile, readlink, symlink, writeFile } from "node:fs/promises"
import { z } from "zod"
import { verificationComputer } from "../agent/task"

export const persistenceAgent = agent({
  id: "verification-persistence", computer: verificationComputer, maxTurnDuration: "10m",
  setup: () => ({ nonce: randomUUID(), marker: "", count: 0 }),
  turn: async (turn, ctx) => {
    const { marker, awaitStart, holdForObservation } = z.object({ marker: z.string().min(1), awaitStart: z.boolean().default(false), holdForObservation: z.boolean().default(false) }).strict().parse(fixtureValue(turn.input))
    const state = ctx.setupResult
    const file = `/root/verification-${state.nonce}.json`
    if (++state.count === 1) {
      state.marker = marker
      await writeFile(file, JSON.stringify({ marker, nonce: state.nonce }), { flag: "wx" })
      await symlink(file, `${file}.link`)
    } else if (state.count !== 2 || state.marker !== marker) {
      throw new Error("Session memory or Turn sequence changed")
    }
    if (await readlink(`${file}.link`) !== file) throw new Error("persisted symlink changed")
    const restored = JSON.parse(await readFile(`${file}.link`, "utf8"))
    if (restored.marker !== marker || restored.nonce !== state.nonce) throw new Error("persisted state changed")
    const result = { marker, nonce: state.nonce, count: state.count, turnId: turn.id, sessionId: ctx.session.id, computerId: ctx.computer.id }
    if (awaitStart) {
      let start!: () => void
      const ready = new Promise<void>(resolve => { start = resolve })
      await turn.onMessage(message => { if (fixtureValue(message) === "start") start() })
      await turn.output.write([{ type: "json", value: { ...result, phase: "ready" } }])
      // Admit every shared member before either first Turn can finish.
      await ready
    }
    if (holdForObservation) {
      let finish!: () => void
      const observed = new Promise<void>(resolve => { finish = resolve })
      await turn.onMessage(message => { if (fixtureValue(message) === "finish") finish() })
      await turn.output.write([{ type: "json", value: { ...result, phase: "restored" } }])
      // Keep the restored allocation active while its exact lineage is observed.
      await observed
    }
    return result
  },
})
