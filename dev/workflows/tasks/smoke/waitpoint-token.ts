import { cache, image, logger, sandbox, source, task, wait } from "@helmr/sdk"
import { appendFile, readFile, writeFile } from "node:fs/promises"
import { z } from "zod"

const dependencyInputs = source.directory(".", {
  ignore: ["*", "!package.json", "!bun.lock", "!tsconfig.json", "!vendor", "!vendor/**"],
})

const base = image("helmr-waitpoint-checkpoint-smoke")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .copy("/opt/helmr-task", dependencyInputs)
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .workdir("/opt/helmr-task")
  .run(["bun", "install", "--frozen-lockfile"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("waitpoint-checkpoint-smoke-bun") }],
  })
  .workdir("/workspace")

const sbx = sandbox("helmr-waitpoint-checkpoint-smoke")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi" })

interface Payload {
  readonly marker?: string
  readonly approvalTimeout?: number
  readonly messageTimeout?: number
}

const payload = z.object({
  marker: z.string().optional(),
  approvalTimeout: z.number().int().positive().optional(),
  messageTimeout: z.number().int().positive().optional(),
}).strict()

interface DiagnosticState {
  readonly marker: string
  readonly cwd: string
  readonly pid: number
  readonly steps: readonly string[]
  readonly decision?: {
    readonly approved: boolean
  }
  readonly message?: {
    readonly text: string
  }
}

const statePath = "waitpoint-checkpoint-smoke-state.json"
const logPath = "waitpoint-checkpoint-smoke.log"
const reportPath = "waitpoint-checkpoint-smoke-report.json"

const approvalDecision = z.object({
  approved: z.boolean(),
})

const messageDecision = z.object({
  text: z.string(),
})

export const waitpointCheckpointSmoke = task({
  id: "waitpoint-checkpoint-smoke",
  sandbox: sbx,
  maxDuration: 1200,
  payload,
  run: async (payload: Payload, ctx) => {
    const marker = payload.marker?.trim() || `waitpoint-checkpoint-${ctx.run.id}`
    const memoryState = {
      marker,
      pid: process.pid,
      steps: ["start"],
    }

    await appendLog("start", memoryState)
    await writeState({
      marker,
      cwd: process.cwd(),
      pid: process.pid,
      steps: memoryState.steps,
    })
    logger.info({ phase: "waitpoint-checkpoint-smoke", step: "before-decision", marker, pid: process.pid })

    const decisionToken = await wait.createToken({
      timeout: payload.approvalTimeout ?? 900,
      tags: ["smoke", "checkpoint"],
      metadata: { marker, subject: `Approve checkpoint diagnostic marker ${marker}` },
    })
    const decision = await wait.forToken(decisionToken, {
      schema: approvalDecision,
      timeout: payload.approvalTimeout ?? 900,
      tags: ["smoke", "checkpoint"],
      metadata: { marker, subject: `Approve checkpoint diagnostic marker ${marker}` },
    }).unwrap()
    memoryState.steps.push("after-decision")
    await assertRestoredState(marker, memoryState.pid, ["start"])
    await appendLog("after-decision", { memoryState, decision })

    if (!decision.approved) {
      const denied = {
        marker,
        approved: false,
        steps: memoryState.steps,
      }
      await writeJson(reportPath, denied)
      return denied
    }

    await writeState({
      marker,
      cwd: process.cwd(),
      pid: process.pid,
      steps: memoryState.steps,
      decision: {
        approved: decision.approved,
      },
    })
    logger.info({ phase: "waitpoint-checkpoint-smoke", step: "before-input", marker, pid: process.pid })

    const replyToken = await wait.createToken({
      timeout: payload.messageTimeout ?? 900,
      tags: ["smoke", "checkpoint"],
      metadata: { marker, subject: `Reply with text for checkpoint diagnostic marker ${marker}` },
    })
    const reply = await wait.forToken(replyToken, {
      schema: messageDecision,
      timeout: payload.messageTimeout ?? 900,
      tags: ["smoke", "checkpoint"],
      metadata: { marker, subject: `Reply with text for checkpoint diagnostic marker ${marker}` },
    }).unwrap()
    memoryState.steps.push("after-input")
    await assertRestoredState(marker, memoryState.pid, ["start", "after-decision"])
    await appendLog("after-input", { memoryState, reply })

    const report = {
      marker,
      cwd: process.cwd(),
      pid: process.pid,
      approved: true,
      messageText: reply.text,
      steps: memoryState.steps,
      files: {
        state: statePath,
        log: logPath,
        report: reportPath,
      },
    }
    await writeState({
      marker,
      cwd: process.cwd(),
      pid: process.pid,
      steps: memoryState.steps,
      decision: {
        approved: decision.approved,
      },
      message: {
        text: reply.text,
      },
    })
    await writeJson(reportPath, report)
    logger.info({ phase: "waitpoint-checkpoint-smoke", step: "completed", marker, pid: process.pid })
    return report
  },
})

async function assertRestoredState(marker: string, pid: number, expectedSteps: readonly string[]): Promise<void> {
  const state = await readJson<DiagnosticState>(statePath)
  if (state.marker !== marker) {
    throw new Error(`state marker mismatch: ${state.marker} != ${marker}`)
  }
  if (state.pid !== pid || process.pid !== pid) {
    throw new Error(`process pid changed across checkpoint: state=${state.pid} memory=${pid} current=${process.pid}`)
  }
  for (const step of expectedSteps) {
    if (!state.steps.includes(step)) {
      throw new Error(`state file is missing step ${step}`)
    }
  }
}

async function appendLog(step: string, value: unknown): Promise<void> {
  await appendFile(logPath, `${JSON.stringify({ step, at: new Date().toISOString(), value })}\n`)
}

async function writeState(state: DiagnosticState): Promise<void> {
  await writeJson(statePath, state)
}

async function writeJson(path: string, value: unknown): Promise<void> {
  await writeFile(path, `${JSON.stringify(value, null, 2)}\n`)
}

async function readJson<T>(path: string): Promise<T> {
  return JSON.parse(await readFile(path, "utf8")) as T
}
