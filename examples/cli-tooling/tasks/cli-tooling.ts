import { image, sandbox, task } from "@helmr/sdk"

const base = image("cli-tooling")
  .from("debian:trixie-slim")
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
    const proc = Bun.spawn(["rg", "--json", pattern, "tasks"], {
      stdout: "pipe",
      stderr: "pipe",
    })
    const [stdout, stderr, exitCode] = await Promise.all([
      new Response(proc.stdout).text(),
      new Response(proc.stderr).text(),
      proc.exited,
    ])
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
    await Bun.write("cli-tooling-report.json", `${JSON.stringify(report, null, 2)}\n`)
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
