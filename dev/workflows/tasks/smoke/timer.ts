import { cache, image, logger, sandbox, source, task, timers } from "@helmr/sdk"
import { readFile, writeFile } from "node:fs/promises"
import { z } from "zod"

const dependencyInputs = source.directory(".", {
  ignore: ["*", "!package.json", "!bun.lock", "!tsconfig.json", "!vendor", "!vendor/**"],
})

const base = image("helmr-timer-smoke")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .copy("/opt/helmr-task", dependencyInputs)
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .workdir("/opt/helmr-task")
  .run(["bun", "install", "--frozen-lockfile"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("timer-smoke-bun") }],
  })
  .workdir("/workspace")

const sbx = sandbox("helmr-timer-smoke")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi" })

const payload = z.object({
  marker: z.string().optional(),
  waitFor: z.string().default("5s"),
}).strict()

type Payload = z.infer<typeof payload>

const statePath = "timer-smoke-state.json"

export const timerSmoke = task({
  id: "timer-smoke",
  sandbox: sbx,
  maxDuration: 300,
  payload,
  run: async (payload: Payload, ctx) => {
    const marker = payload.marker?.trim() || `timer-smoke-${ctx.run.id}`
    const before = {
      marker,
      pid: process.pid,
      steps: ["before-timer"],
    }
    await writeFile(statePath, `${JSON.stringify(before, null, 2)}\n`)
    logger.info({ phase: "timer-smoke", step: "before-timer", marker, waitFor: payload.waitFor })

    const startedAt = Date.now()
    await timers.waitFor(payload.waitFor).unwrap()
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
    logger.info({ phase: "timer-smoke", step: "completed", marker, elapsedMs })
    return report
  },
})
