import { fixtureValue } from "../../support/runtime-mcp"
import { agent, computer, image } from "@helmr/sdk"
import { randomUUID } from "node:crypto"
import { writeFileSync } from "node:fs"
import { readFile, writeFile, access } from "node:fs/promises"
import { z } from "zod"

export const captureAbortComputer = computer({
  id: "capture-abort-verification",
  image: image("capture-abort-verification").from("node:24-bookworm-slim").workdir("/workspace"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const captureAbortAgent = agent({
  id: "verification-capture-abort", computer: captureAbortComputer, maxTurnDuration: "20m",
  setup: () => ({ nonce: randomUUID(), count: 0, marker: "" }),
  turn: async (turn, ctx) => {
    const { marker, cancelledMarker, cancelMember, cancelledCounter } = z.object({
      marker: z.string(), cancelledMarker: z.string(), cancelMember: z.boolean(),
      cancelledCounter: z.number().int().positive().optional(),
    }).strict().parse(fixtureValue(turn.input))
    const memory = ctx.setupResult
    const count = ++memory.count
    const file = `/workspace/capture-abort-${memory.nonce}`
    const counterFile = `${cancelledMarker}.counter`
    const state = () => ({ marker, nonce: memory.nonce, count, turnId: turn.id, sessionId: ctx.session.id, computerId: ctx.computer.id })
    if (cancelMember) {
      // A queued Turn admitted while frozen must never enter after cancellation.
      writeFileSync(counterFile, String(count))
      // Input identifies queued work even if an incorrect cold start resets memory.
      if (cancelledCounter !== undefined || count > 1) {
        writeFileSync(cancelledMarker, "cancelled queued Turn entered")
        throw new Error("cancelled member executed after capture")
      }
    }
    if (count === 1) {
      memory.marker = marker
      await writeFile(file, marker, { flag: "wx" })
      let start!: () => void
      const armed = new Promise<void>(resolve => { start = resolve })
      await turn.onMessage(message => { if (fixtureValue(message) === "start") start() })
      await turn.output.write([{ type: "json", value: { ...state(), phase: "ready" } }])
      await armed
      // Only a completed Turn with no authored timers/I/O can become capturable.
      return state()
    }
    if (memory.marker !== marker || count > 3 || cancelledCounter === undefined) throw new Error("Session state changed")
    try {
      await access(cancelledMarker)
      throw new Error("cancelled queued Turn left its execution marker")
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error
    }
    const observed = z.number().int().positive().parse(JSON.parse(await readFile(counterFile, "utf8")))
    if (observed !== cancelledCounter) throw new Error(`cancelled Turn counter changed: ${cancelledCounter} -> ${observed}`)
    if (await readFile(file, "utf8") !== marker) throw new Error("capture lost the file")
    let finish!: () => void
    const inspected = new Promise<void>(resolve => { finish = resolve })
    await turn.onMessage(message => { if (fixtureValue(message) === "finish") finish() })
    await turn.output.write([{ type: "json", value: { ...state(), phase: count === 2 ? "resumed" : "restored" } }])
    // Keep this allocation active until the driver observes its exact checkpoint.
    await inspected
    return state()
  },
})
