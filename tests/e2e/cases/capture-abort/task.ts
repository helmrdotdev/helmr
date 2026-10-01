import { task, tokens, logger, sandbox } from "@helmr/sdk"
import { randomUUID } from "node:crypto"
import { writeFileSync } from "node:fs"
import { readFile, writeFile, access } from "node:fs/promises"
import { z } from "zod"

export const captureAbortSandbox = sandbox({ id: "capture-abort-verification" })

export const captureAbortTask = task({
  id: "verification-capture-abort",
  maxDuration: "20m",
  retry: { enabled: false },
  payload: z.object({ marker: z.string(), first: z.string(), second: z.string(), cancelledMarker: z.string(), cancelMember: z.boolean() }).strict(),
  run: async ({ marker, first, second, cancelledMarker, cancelMember }, ctx) => {
    const nonce = randomUUID()
    const file = `/root/capture-abort-${nonce}`
    await writeFile(file, marker, { flag: "wx" })
    const state = () => ({ marker, nonce, runId: ctx.run.id, computerId: ctx.computer.id })
    await logger.info("capture abort before", { ...state(), phase: "before" })
    const counterFile = `${cancelledMarker}.counter`
    if (cancelMember) {
      let counter = 0
      writeFileSync(counterFile, String(counter), { flag: "wx" })
      setInterval(() => {
        counter++
        // Write before emitting the baseline evidence. A buffered final log can
        // only make qualification fail conservatively, never hide execution.
        writeFileSync(counterFile, String(counter))
        void logger.info("capture cancellation probe", { marker, counter })
      }, 25)
    }
    let firstResult: unknown
    try {
      firstResult = await tokens.ref(first).wait({ timeout: "12m", idleTimeout: "2s" }).unwrap()
    } finally {
      // Ordinary cancellation kills the process while frozen; it cannot run
      // JavaScript cleanup. This also detects a rejected wait reaching user code.
      if (cancelMember) writeFileSync(cancelledMarker, "cancelled wait continued")
    }
    if (cancelMember) {
      throw new Error("cancelled member resumed")
    }
    const assertCancelledMemberStopped = async () => {
      try { await access(cancelledMarker) }
      catch (error) {
        if ((error as NodeJS.ErrnoException).code === "ENOENT") return
        throw error
      }
      throw new Error("cancelled member executed after capture")
    }
    const baseline = z.object({ cancelledCounter: z.number().int().positive() }).strict().parse(firstResult)
    const assertCancelledCounterUnchanged = async () => {
      const observed = z.number().int().nonnegative().parse(JSON.parse(await readFile(counterFile, "utf8")))
      if (observed !== baseline.cancelledCounter) throw new Error(`cancelled execution counter changed: ${baseline.cancelledCounter} -> ${observed}`)
    }
    await assertCancelledCounterUnchanged()
    await assertCancelledMemberStopped()
    if (await readFile(file, "utf8") !== marker) throw new Error("file changed after abort")
    await logger.info("capture abort resumed", { ...state(), phase: "resumed" })
    await tokens.ref(second).wait({ timeout: "6m", idleTimeout: "30s" }).unwrap()
    await assertCancelledCounterUnchanged()
    await assertCancelledMemberStopped()
    if (await readFile(file, "utf8") !== marker) throw new Error("file changed after restore")
    return state()
  },
})
