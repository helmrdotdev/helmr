import { fixtureValue } from "../../support/runtime-mcp"
import { agent } from "@helmr/sdk"
import { z } from "zod"
import { verificationComputer } from "../agent/task"

export const networkAgent = agent({
  id: "verification-network", computer: verificationComputer, maxTurnDuration: "10m",
  turn: async (turn, ctx) => {
    const { marker } = z.object({ marker: z.string().min(1) }).strict().parse(fixtureValue(turn.input))
    let start!: () => void, finish!: () => void
    const started = new Promise<void>(resolve => { start = resolve })
    const finished = new Promise<void>(resolve => { finish = resolve })
    await turn.onMessage(value => {
      const message = z.object({ phase: z.enum(["start", "finish"]) }).strict().parse(fixtureValue(value))
      if (message.phase === "start") start()
      else finish()
    })
    const positive = await fetch("https://example.com/", { signal: AbortSignal.any([turn.signal, AbortSignal.timeout(15_000)]) })
    if (!positive.ok) throw new Error(`public HTTPS control failed: ${positive.status}`)
    await positive.arrayBuffer()
    // Application message gates keep this Turn active while the driver reads
    // counters in this exact VM.
    await turn.output.write([{ type: "json", value: { phase: "ready", marker } }])
    await started
    let blocked = false
    try {
      // Probe only the metadata index, never credentials or an IMDS token.
      const response = await fetch("http://169.254.169.254/latest/meta-data/", { signal: AbortSignal.any([turn.signal, AbortSignal.timeout(5000)]) })
      await response.body?.cancel()
    } catch { turn.signal.throwIfAborted(); blocked = true }
    if (!blocked) throw new Error("metadata endpoint returned an HTTP response")
    await turn.output.write([{ type: "json", value: { phase: "observed", marker, positiveStatus: positive.status, blocked } }])
    await finished
    return { marker, blocked, positiveStatus: positive.status, turnId: turn.id, sessionId: ctx.session.id, computerId: ctx.computer.id }
  },
})
