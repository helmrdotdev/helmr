import { spawn } from "node:child_process"
import { readdir, readFile } from "node:fs/promises"
import { setTimeout as delay } from "node:timers/promises"

import type { HumanContent, InputContent } from "@helmr/sdk"

export function issuePrompt(input: InputContent): HumanContent {
  return [{ type: "text", text: "Fix this issue and explain the change: " }, ...input]
}

// Configure these on the application host, never from webhook JSON or model output.
export function repository(): string {
  const cwd = process.env.ISSUE_FIXER_REPOSITORY
  if (!cwd) throw new Error("Set ISSUE_FIXER_REPOSITORY to a disposable checked-out repository")
  return cwd
}

export async function checkRepository(cwd: string, signal: AbortSignal): Promise<void> {
  signal.throwIfAborted()
  // Edit this fixed command to match the target repository's deterministic checks.
  // A Linux process group lets cancellation reach npm's test runner as well.
  const child = spawn("npm", ["test", "--", "--runInBand"], { cwd, detached: true, stdio: "inherit" })
  let failure: Error | undefined
  const cancel = () => {
    if (child.pid === undefined) return
    try { process.kill(-child.pid, "SIGTERM") } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ESRCH") failure = error as Error
    }
  }
  signal.addEventListener("abort", cancel, { once: true })
  try {
    const code = await new Promise<number | null>((resolve) => {
      child.once("error", (error) => { failure = error })
      child.once("close", resolve)
      if (signal.aborted) cancel()
    })
    // npm may exit before its descendants. Linux can retain their zombies
    // under the Session's Node PID 1; only live group members keep work pending.
    // Helmr owns the bounded stop fallback if a member refuses to stop.
    while (signal.aborted && child.pid !== undefined) {
      let running = false
      let names: string[]
      try { names = await readdir("/proc") } catch { await delay(25); continue }
      for (const name of names) {
        if (!/^\d+$/.test(name)) continue
        try {
          const stat = await readFile(`/proc/${name}/stat`, "utf8")
          const fields = stat.slice(stat.lastIndexOf(")") + 2).split(" ")
          if (Number(fields[2]) === child.pid && fields[0] !== "Z" && fields[0] !== "X") running = true
        } catch (error) {
          // A disappearing process is gone; unreadable state cannot prove it.
          if ((error as NodeJS.ErrnoException).code !== "ENOENT") running = true
        }
      }
      if (!running) break
      await delay(25)
    }
    signal.throwIfAborted()
    if (failure) throw failure
    if (code !== 0) throw new Error(`Repository checks failed (${code})`)
  } finally {
    signal.removeEventListener("abort", cancel)
  }
}
