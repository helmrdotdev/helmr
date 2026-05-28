import { Agent, type Run, type RunResult } from "@cursor/sdk"
import { baseAgentEnv, withProcessEnv } from "../env"
import type { Input } from "../types"

export async function runCursor(input: Input, cursorApiKey: string, prompt: string): Promise<string> {
  const maxAttempts = 3
  for (let attempt = 1; attempt <= maxAttempts; attempt += 1) {
    try {
      return await runCursorOnce(input, cursorApiKey, prompt)
    } catch (error) {
      if (attempt >= maxAttempts || !isTransientCursorError(error)) throw error
      await delay(1000 * attempt * attempt)
    }
  }
  throw new Error("Cursor SDK run ended without a result")
}

async function runCursorOnce(input: Input, cursorApiKey: string, prompt: string): Promise<string> {
  return withProcessEnv(cursorAgentEnv(cursorApiKey), async () => {
    const agent = await Agent.create({
      apiKey: cursorApiKey,
      model: { id: input.cursorModel },
      local: { cwd: process.cwd() },
    })
    try {
      const run = await agent.send(prompt, {
        model: { id: input.cursorModel },
        local: { force: true },
      })
      const result = await waitForCursorRun(run)
      if (result.status !== "finished") {
        throw new Error(`Cursor SDK run ${result.id} ended with status ${result.status}`)
      }
      if (!result.result?.trim()) {
        throw new Error(`Cursor SDK run ${result.id} finished without a text result`)
      }
      return result.result.trim()
    } finally {
      agent.close()
    }
  })
}

async function waitForCursorRun(run: Run): Promise<RunResult> {
  try {
    return await run.wait()
  } catch (error) {
    if (!isTransientCursorError(error)) throw error
    console.warn(`Cursor SDK wait for run ${run.id} hit a transient error; polling run status`)
    return pollCursorRun(run)
  }
}

async function pollCursorRun(run: Run): Promise<RunResult> {
  const deadline = Date.now() + 60 * 60 * 1000
  let lastError: unknown
  while (Date.now() < deadline) {
    try {
      const current = await Agent.getRun(run.id, { cwd: process.cwd() })
      if (current.status !== "running") {
        return {
          id: current.id,
          status: current.status,
          result: current.result,
          model: current.model,
          durationMs: current.durationMs,
          git: current.git,
        }
      }
      lastError = undefined
    } catch (error) {
      if (!isTransientCursorError(error)) throw error
      lastError = error
    }
    await delay(15_000)
  }
  const detail = lastError instanceof Error ? `: ${lastError.message}` : ""
  throw new Error(`Timed out polling Cursor SDK run ${run.id}${detail}`)
}

function isTransientCursorError(error: unknown): boolean {
  const message = error instanceof Error ? error.message : String(error)
  return /\b(ETIMEDOUT|ECONNRESET|ECONNREFUSED|EAI_AGAIN|ENETUNREACH)\b/i.test(message)
    || /\[(unavailable|deadline_exceeded|resource_exhausted)\]/i.test(message)
}

function cursorAgentEnv(cursorApiKey: string): Record<string, string> {
  return {
    ...baseAgentEnv(),
    CURSOR_API_KEY: cursorApiKey,
  }
}

function delay(milliseconds: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, milliseconds))
}
