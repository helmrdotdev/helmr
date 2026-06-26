import { cache, image, logger, sandbox, source, streams, task } from "@helmr/sdk"
import { appendFile, writeFile } from "node:fs/promises"
import { z } from "zod"

const dependencyInputs = source.directory(".", {
  ignore: ["*", "!package.json", "!bun.lock", "!tsconfig.json", "!vendor", "!vendor/**"],
})

const base = image("helmr-session-continuation-smoke")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .copy("/opt/helmr-task", dependencyInputs)
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .workdir("/opt/helmr-task")
  .run(["bun", "install", "--frozen-lockfile"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("session-continuation-smoke-bun") }],
  })
  .workdir("/workspace")

const sbx = sandbox("helmr-session-continuation-smoke")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi" })

const payload = z.object({
  marker: z.string().optional(),
  correlationId: z.string().optional(),
}).strict()

type Payload = z.infer<typeof payload>

const input = streams.input("session-continuation-smoke.input", {
  schema: z.object({
    message: z.string().min(1),
  }).strict(),
})
const reportStream = streams.output("session-continuation-smoke.report", { schema: z.unknown() })

const logPath = "session-continuation-smoke.log"
const reportPath = "session-continuation-smoke-report.json"

export const sessionContinuationSmoke = task({
  id: "session-continuation-smoke",
  sandbox: sbx,
  maxDuration: 600,
  payload,
  run: async (payload: Payload, ctx) => {
    const marker = payload.marker?.trim() || `session-continuation-smoke-${ctx.run.id}`
    const correlationId = payload.correlationId?.trim() || marker
    const record = await input.peek({ correlationId })
    const phase = record === null ? "initial-idle" : "continuation"
    const report = {
      ok: true,
      phase,
      marker,
      correlationId,
      runId: ctx.run.id,
      pid: process.pid,
      input: record === null ? null : {
        id: record.id,
        sequence: record.sequence,
        data: record.data,
      },
      files: {
        log: logPath,
        report: reportPath,
      },
    }
    await appendLog(phase, report)
    await writeFile(reportPath, `${JSON.stringify(report, null, 2)}\n`)
    await reportStream.append(report, { contentType: "application/vnd.helmr.session-continuation-smoke+json" })
    logger.info({ phase: "session-continuation-smoke", marker, step: phase })
    return report
  },
})

async function appendLog(step: string, value: unknown): Promise<void> {
  await appendFile(logPath, `${JSON.stringify({ step, at: new Date().toISOString(), value })}\n`)
}
