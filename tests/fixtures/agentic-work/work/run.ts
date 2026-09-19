import { spawn } from "node:child_process"

export type Finished = { code: number | null; signal: string | null; stdout: string; stderr: string }

// Runs one child to completion and keeps both streams.
export function run(command: string, args: string[], options: { cwd?: string; input?: string } = {}): Promise<Finished> {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { cwd: options.cwd, stdio: ["pipe", "pipe", "pipe"] })
    let stdout = ""
    let stderr = ""
    child.stdout.on("data", chunk => { stdout += chunk })
    child.stderr.on("data", chunk => { stderr += chunk })
    child.on("error", reject)
    child.on("close", (code, signal) => resolve({ code, signal, stdout, stderr }))
    child.stdin.end(options.input ?? "")
  })
}

export async function succeed(command: string, args: string[], cwd?: string): Promise<string> {
  const result = await run(command, args, { cwd })
  if (result.code !== 0) {
    throw new Error(`${command} ${args.join(" ")} exited ${result.code}: ${result.stderr}`)
  }
  return result.stdout
}
