import { spawn } from "node:child_process"

export type Finished = {
  code: number | null
  signal: string | null
  stdout: string
  stderr: string
  // Why the input could not be delivered (the child closed its stdin or exited
  // first), or null. The child's own exit code and stderr are reported either way.
  stdinError: string | null
}

// Runs one child to completion and keeps both streams. A child gets a stdin
// pipe only when there is input for it: a quick command such as `git config`
// can exit before its parent writes, and a write to that closed pipe, even an
// empty one, fails with EPIPE.
export function run(command: string, args: string[], options: { cwd?: string; input?: string } = {}): Promise<Finished> {
  return new Promise((resolve, reject) => {
    const input = options.input
    const child = spawn(command, args, { cwd: options.cwd, stdio: [input === undefined ? "ignore" : "pipe", "pipe", "pipe"] })
    let stdout = ""
    let stderr = ""
    let stdinError: string | null = null
    child.stdout!.on("data", chunk => { stdout += chunk })
    child.stderr!.on("data", chunk => { stderr += chunk })
    child.on("error", reject)
    child.on("close", (code, signal) => resolve({ code, signal, stdout, stderr, stdinError }))
    if (child.stdin !== null) {
      child.stdin.on("error", error => { stdinError = error.message })
      child.stdin.end(input)
    }
  })
}

export async function succeed(command: string, args: string[], cwd?: string): Promise<string> {
  const result = await run(command, args, { cwd })
  if (result.code !== 0) {
    throw new Error(`${command} ${args.join(" ")} exited ${result.code}: ${result.stderr}`)
  }
  return result.stdout
}
