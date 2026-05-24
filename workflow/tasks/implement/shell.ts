import { compactEnv } from "./env"
import { spawn } from "node:child_process"

export async function commandExists(command: string): Promise<void> {
  await run(["sh", "-ceu", `command -v "$1" >/dev/null`, "sh", command], {
    label: `command -v ${command}`,
    env: {
      ...compactEnv(process.env),
      PATH: `/root/.local/bin:${process.env.PATH ?? ""}`,
    },
  }).catch((error: unknown) => {
    throw new Error(
      [
        `${command} is required but was not found.`,
        "Install Cursor CLI with the official installer in the task image, or pass payload.cursorCommand.",
        error instanceof Error ? error.message : String(error),
      ].join("\n"),
    )
  })
}

export async function run(
  command: readonly string[],
  opts: {
    readonly label?: string
    readonly env?: Record<string, string>
  } = {},
): Promise<string> {
  return new Promise((resolve, reject) => {
    const proc = spawn(command[0] ?? "", command.slice(1), {
      env: opts.env,
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
        reject(new Error(`${opts.label ?? command.join(" ")} exited ${exitCode}: ${stderr}`))
        return
      }
      resolve(stdout)
    })
  })
}

export function parseJson<T>(value: string): T {
  const trimmed = value.trim()
  try {
    return JSON.parse(trimmed) as T
  } catch {
    const fenced = trimmed.match(/```(?:json)?\s*([\s\S]*?)```/i)
    if (fenced?.[1]) {
      return JSON.parse(fenced[1]) as T
    }
    throw new Error(`Expected JSON output but received: ${trimmed.slice(0, 500)}`)
  }
}
