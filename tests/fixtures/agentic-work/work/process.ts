import { spawn } from "node:child_process"
import { readFile, readdir } from "node:fs/promises"
import { dirname, join } from "node:path"
import { fileURLToPath } from "node:url"
import { run } from "./run.ts"

const tool = join(dirname(fileURLToPath(import.meta.url)), "tool.mjs")
const marker = "agentic-hang-marker"

async function markedProcesses(): Promise<number> {
  let count = 0
  for (const entry of await readdir("/proc")) {
    if (!/^\d+$/.test(entry) || Number(entry) === process.pid) continue
    const command = await readFile(`/proc/${entry}/cmdline`, "utf8").catch(() => "")
    if (command.includes(marker)) count++
  }
  return count
}

// A tool that never answers, and that started a helper of its own. The caller
// bounds the wait and removes the whole process group.
async function boundedHang(limitMs: number) {
  const started = Date.now()
  const child = spawn(process.execPath, [tool, "hang", marker], { detached: true, stdio: ["ignore", "pipe", "ignore"] })
  await new Promise(resolve => child.stdout.once("data", resolve))
  const whileRunning = await markedProcesses()
  const outcome = await new Promise<{ timedOut: boolean; signal: string | null }>(resolve => {
    const timer = setTimeout(() => process.kill(-child.pid!, "SIGKILL"), limitMs)
    child.on("close", (_code, signal) => {
      clearTimeout(timer)
      resolve({ timedOut: signal === "SIGKILL", signal })
    })
  })
  // The group is gone once its members have been reaped by this namespace's init.
  let lingering = await markedProcesses()
  for (let attempt = 0; lingering !== 0 && attempt < 50; attempt++) {
    await new Promise(resolve => setTimeout(resolve, 20))
    lingering = await markedProcesses()
  }
  return { ...outcome, whileRunning, lingering, elapsedMs: Date.now() - started }
}

export async function processWork() {
  const exchange = await run(process.execPath, [tool, "sum"], { input: JSON.stringify({ values: [4, 8, 15, 16, 23, 42] }) })
  if (exchange.stdinError !== null) {
    throw new Error(`the tool did not take its request: ${exchange.stdinError}; exited ${exchange.code}: ${exchange.stderr}`)
  }
  const failure = await run(process.execPath, [tool, "fail"])
  return {
    exchange: { code: exchange.code, reply: JSON.parse(exchange.stdout), stderr: exchange.stderr },
    failure: { code: failure.code, stdout: failure.stdout, stderr: failure.stderr.trim() },
    hang: await boundedHang(1000),
  }
}
