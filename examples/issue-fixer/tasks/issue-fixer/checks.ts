import { spawn } from "node:child_process"
import { z } from "zod"

export const issueSchema = z.object({ issue: z.string().min(1) })

// Configure these on the application host, never from webhook JSON or model output.
export function repository(): string {
  const cwd = process.env.ISSUE_FIXER_REPOSITORY
  if (!cwd) throw new Error("Set ISSUE_FIXER_REPOSITORY to a disposable checked-out repository")
  return cwd
}

export async function checkRepository(cwd: string, signal: AbortSignal): Promise<void> {
  signal.throwIfAborted()
  // Edit this fixed command to match the target repository's deterministic checks.
  const child = spawn("npm", ["test", "--", "--runInBand"], { cwd, signal, stdio: "inherit" })
  await new Promise<void>((resolve, reject) => {
    child.once("error", reject)
    child.once("close", code => code === 0 ? resolve() : reject(new Error(`Repository checks failed (${code})`)))
  })
}
