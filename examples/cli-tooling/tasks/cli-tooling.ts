import { cache, image, sandbox, source, task } from "@helmr/sdk"
import { spawn } from "node:child_process"
import { writeFile } from "node:fs/promises"

const base = image("cli-tooling")
  .from("oven/bun:1.3.10-debian")
  .workdir("/workspace")
  .copy("/workspace/package.json", source.file("package.json"))
  .run(["bun", "install"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("cli-tooling-bun") }],
  })
  .run([
    "sh",
    "-ceu",
    "apt-get update && apt-get install -y --no-install-recommends ripgrep && rm -rf /var/lib/apt/lists/*",
  ])

const sbx = sandbox("cli-tooling")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi" })

interface Payload {
  readonly pattern?: string
}

export const cliTooling = task({
  id: "cli-tooling",
  sandbox: sbx,
  maxDuration: 300,
  run: async (payload: Payload, ctx) => {
    const pattern = payload.pattern?.trim() || "export const"
    const { stdout, stderr, exitCode } = await runCommand(["rg", "--json", pattern, "tasks"])
    if (exitCode !== 0) {
      throw new Error(`rg exited ${exitCode}: ${stderr}`)
    }

    const matches = stdout
      .trim()
      .split("\n")
      .filter(Boolean)
      .map((line) => JSON.parse(line) as RipgrepEvent)
      .filter((event): event is RipgrepMatch => event.type === "match")
      .map((event) => ({
        path: event.data.path.text,
        line: event.data.line_number,
        text: event.data.lines.text.trim(),
      }))

    const report = { runId: ctx.run.id, tool: "ripgrep", pattern, matches }
    await writeFile("cli-tooling-report.json", `${JSON.stringify(report, null, 2)}\n`)
    ctx.log.info({ report: "cli-tooling-report.json", matches: matches.length })
    return report
  },
})

type RipgrepEvent = RipgrepMatch | { readonly type: string }

interface RipgrepMatch {
  readonly type: "match"
  readonly data: {
    readonly path: { readonly text: string }
    readonly line_number: number
    readonly lines: { readonly text: string }
  }
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
