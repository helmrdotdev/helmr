import { cache, image, sandbox, source, task } from "@helmr/sdk"
import { appendFile, readFile, writeFile } from "node:fs/promises"

const dependencyInputs = source.directory(".", {
  ignore: ["*", "!package.json", "!bun.lock", "!tsconfig.json"],
})

const base = image("helmr-checkpoint-waitpoint-debug")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .copy("/workspace", dependencyInputs)
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .run(["bun", "install", "--frozen-lockfile"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("checkpoint-waitpoint-debug-bun") }],
  })

const sbx = sandbox("helmr-checkpoint-waitpoint-debug")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi" })

interface Payload {
  readonly marker?: string
  readonly approvalTimeout?: number
  readonly messageTimeout?: number
}

interface DebugState {
  readonly marker: string
  readonly cwd: string
  readonly pid: number
  readonly steps: readonly string[]
  readonly approval?: {
    readonly approved: boolean
    readonly approvedBy: string
    readonly at: string
  }
  readonly message?: {
    readonly text: string
    readonly sentBy: string
    readonly at: string
  }
}

const statePath = "checkpoint-waitpoint-debug-state.json"
const logPath = "checkpoint-waitpoint-debug.log"
const reportPath = "checkpoint-waitpoint-debug-report.json"

export const checkpointWaitpointDebug = task({
  id: "checkpoint-waitpoint-debug",
  sandbox: sbx,
  maxDuration: 1200,
  run: async (payload: Payload, ctx) => {
    const marker = payload.marker?.trim() || `checkpoint-debug-${ctx.run.id}`
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
    ctx.log.info({ phase: "checkpoint-waitpoint-debug", step: "before-approval", marker, pid: process.pid })

    const approval = await ctx.wait.approval(`Approve checkpoint debug marker ${marker}`, {
      timeout: payload.approvalTimeout ?? 900,
    })
    memoryState.steps.push("after-approval")
    await assertRestoredState(marker, memoryState.pid, ["start"])
    await appendLog("after-approval", { memoryState, approval })

    if (!approval.approved) {
      const denied = {
        marker,
        approved: false,
        approvedBy: approval.approvedBy,
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
      approval: {
        approved: approval.approved,
        approvedBy: approval.approvedBy,
        at: approval.at.toISOString(),
      },
    })
    ctx.log.info({ phase: "checkpoint-waitpoint-debug", step: "before-message", marker, pid: process.pid })

    const reply = await ctx.wait.message(`Reply with text for checkpoint debug marker ${marker}`, {
      timeout: payload.messageTimeout ?? 900,
    })
    memoryState.steps.push("after-message")
    await assertRestoredState(marker, memoryState.pid, ["start", "after-approval"])
    await appendLog("after-message", { memoryState, reply })

    const report = {
      marker,
      cwd: process.cwd(),
      pid: process.pid,
      approved: true,
      approvedBy: approval.approvedBy,
      messageFrom: reply.sentBy,
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
      approval: {
        approved: approval.approved,
        approvedBy: approval.approvedBy,
        at: approval.at.toISOString(),
      },
      message: {
        text: reply.text,
        sentBy: reply.sentBy,
        at: reply.at.toISOString(),
      },
    })
    await writeJson(reportPath, report)
    ctx.log.info({ phase: "checkpoint-waitpoint-debug", step: "completed", marker, pid: process.pid })
    return report
  },
})

async function assertRestoredState(marker: string, pid: number, expectedSteps: readonly string[]): Promise<void> {
  const state = await readJson<DebugState>(statePath)
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

async function writeState(state: DebugState): Promise<void> {
  await writeJson(statePath, state)
}

async function writeJson(path: string, value: unknown): Promise<void> {
  await writeFile(path, `${JSON.stringify(value, null, 2)}\n`)
}

async function readJson<T>(path: string): Promise<T> {
  return JSON.parse(await readFile(path, "utf8")) as T
}
