import { spawn } from "node:child_process"
import { readFile, writeFile } from "node:fs/promises"

export function assert(condition: unknown, message: string): asserts condition {
  if (!condition) {
    throw new Error(message)
  }
}

export async function readText(path: string): Promise<string> {
  return await readFile(path, "utf8")
}

export async function command(args: readonly string[]): Promise<string> {
  const { stdout, stderr, exitCode } = await runCommand(args)
  assert(exitCode === 0, `${args.join(" ")} exited ${exitCode}: ${stderr}`)
  return stdout
}

export async function assertVisibleFile(path: string, expected: string): Promise<void> {
  await writeFile(path, expected)
  assert((await readText(path)) === expected, `${path} was not visible during the run`)
}

function runCommand(command: readonly string[]): Promise<{ stdout: string, stderr: string, exitCode: number | null }> {
  return new Promise((resolve, reject) => {
    const proc = spawn(command[0] ?? "", command.slice(1), { stdio: ["ignore", "pipe", "pipe"] })
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
      resolve({ stdout, stderr, exitCode })
    })
  })
}
