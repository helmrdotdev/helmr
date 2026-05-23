import { compactEnv } from "./env"

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
  const proc = Bun.spawn([...command], {
    stdout: "pipe",
    stderr: "pipe",
    env: opts.env,
  })
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
    proc.exited,
  ])
  if (exitCode !== 0) {
    throw new Error(`${opts.label ?? command.join(" ")} exited ${exitCode}: ${stderr}`)
  }
  return stdout
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
