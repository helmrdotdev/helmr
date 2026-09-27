import { actor, logger, tokens } from "@helmr/sdk"
import { randomUUID } from "node:crypto"
import { readFile, writeFile } from "node:fs/promises"
import { z } from "zod"

// One Actor, two Turns: memory is created here, never supplied by the driver.
export const verificationActor = actor({
  id: "verification-actor",
  maxDuration: "10m",
  retry: { enabled: false },
  idleTimeout: "2s",
  async run(session, ctx) {
    const nonce = randomUUID()
    let count = 0
    let marker: string | undefined
    const file = `/root/verification-actor-${nonce}.json`
    for (;;) {
      const turn = await session.receive()
      if (turn === null) return
      try {
        const input = z.object({ marker: z.string().uuid(), tokenId: z.string().optional() }).strict().parse(turn.input)
        count += 1
        if (count === 1) {
          if (!input.tokenId) throw new Error("first Turn requires its Token")
          marker = input.marker
          await writeFile(file, JSON.stringify({ marker, nonce }), { flag: "wx" })
          await logger.info("verification Actor parked", { marker, nonce, sessionId: session.id, runId: ctx.run.id })
          await tokens.ref(input.tokenId).wait({ timeout: "8m", idleTimeout: "2s" }).unwrap()
        } else if (count !== 2 || input.tokenId || input.marker !== marker) {
          throw new Error("Actor memory or Turn sequence changed")
        }
        const stored = JSON.parse(await readFile(file, "utf8"))
        if (stored.marker !== marker || stored.nonce !== nonce) throw new Error("Actor filesystem state changed")
        await turn.complete({ marker: marker!, nonce, count, sessionId: session.id,
                              runId: ctx.run.id, workspaceId: ctx.workspace.id })
      } catch (error) {
        if (turn.signal.aborted) throw error
        await turn.fail(error)
        throw error
      }
    }
  },
})
