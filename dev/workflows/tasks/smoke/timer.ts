import { image, task, timers, workspace } from "@helmr/sdk"
import { readFile, writeFile } from "node:fs/promises"
import { z } from "zod"

const base = image("helmr-timer-smoke")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .workdir("/workspace")

export const timerSmokeWorkspace = workspace("helmr-timer-smoke")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi", disk: "8Gi" })

const payload = z.object({
  marker: z.string().optional(),
  waitFor: z.string().default("5s"),
}).strict()

type Payload = z.infer<typeof payload>

const statePath = "timer-smoke-state.json"

export const timerSmoke = task({
  id: "timer-smoke",
  maxDuration: "5m",
  payload,
  run: async (payload: Payload, ctx) => {
    const marker = payload.marker?.trim() || `timer-smoke-${ctx.run.id}`
    const before = {
      marker,
      pid: process.pid,
      steps: ["before-timer"],
    }
    await writeFile(statePath, `${JSON.stringify(before, null, 2)}\n`)
    console.info({ phase: "timer-smoke", step: "before-timer", marker, waitFor: payload.waitFor })

    const startedAt = Date.now()
    await timers.waitFor(payload.waitFor)
    const elapsedMs = Date.now() - startedAt

    const restored = JSON.parse(await readFile(statePath, "utf8")) as typeof before
    if (restored.marker !== marker || restored.pid !== before.pid || !restored.steps.includes("before-timer")) {
      throw new Error("timer smoke state did not survive parked wait")
    }
    const report = {
      ok: true,
      marker,
      waitFor: payload.waitFor,
      elapsedMs,
      pid: process.pid,
      steps: [...before.steps, "after-timer"],
    }
    await writeFile("timer-smoke-report.json", `${JSON.stringify(report, null, 2)}\n`)
    console.info({ phase: "timer-smoke", step: "completed", marker, elapsedMs })
    return report
  },
})
