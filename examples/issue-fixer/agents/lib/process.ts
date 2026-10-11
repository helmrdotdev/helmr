import { spawn } from "node:child_process"
import { baseAgentEnv } from "./env"

export interface CommandCheckResult {
  readonly command: string
  readonly ok: true
  readonly output: string
}

export async function checkCommand(
  command: readonly string[],
  env: Record<string, string> = baseAgentEnv(),
): Promise<CommandCheckResult> {
  const output = await runCommand(command, env)
  return {
    command: command.join(" "),
    ok: true,
    output: output.trim().slice(0, 2000),
  }
}

export function runCommand(command: readonly string[], env: Record<string, string> = baseAgentEnv()): Promise<string> {
  return new Promise((resolve, reject) => {
    const proc = spawn(command[0] ?? "", command.slice(1), {
      env,
      stdio: ["ignore", "pipe", "pipe"],
    })
    let stdout = ""
    let stderr = ""
    proc.stdout.setEncoding("utf8")
    proc.stderr.setEncoding("utf8")
    proc.stdout.on("data", (chunk: string) => {
      stdout += chunk
    })
    proc.stderr.on("data", (chunk: string) => {
      stderr += chunk
    })
    proc.on("error", reject)
    proc.on("close", (exitCode) => {
      if (exitCode !== 0) {
        const detail = [stderr.trim(), stdout.trim()].filter(Boolean).join("\n")
        reject(new Error(`${command.join(" ")} exited ${exitCode}: ${detail}`))
        return
      }
      resolve(stdout || stderr)
    })
  })
}
