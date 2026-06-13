import { cache, image, sandbox, source, task } from "@helmr/sdk"
import { createHash, randomUUID } from "node:crypto"
import { mkdir, readFile, stat, writeFile } from "node:fs/promises"
import { checkCommand as checkProcessCommand } from "../lib/process"
import { z } from "zod"

const dependencyInputs = source.directory(".", {
  ignore: ["*", "!package.json", "!bun.lock", "!tsconfig.json"],
})
const guideInputs = source.directory("guides")

const base = image("helmr-runtime-smoke")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .copy("/workspace", dependencyInputs)
  .copy("/opt/helmr-dev-workflows/guides", guideInputs)
  .run([
    "sh",
    "-ceu",
    [
      "apt-get update",
      "apt-get install -y --no-install-recommends ca-certificates git jq ripgrep",
      "rm -rf /var/lib/apt/lists/*",
    ].join(" && "),
  ])
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .run(["bun", "install", "--frozen-lockfile"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("runtime-smoke-bun") }],
  })

const sbx = sandbox("helmr-runtime-smoke")
  .image(base)
  .resources({ cpu: 2, memory: "2Gi", disk: "16Gi" })

const payload = z.object({
  scenario: z.string().default("release-smoke"),
  marker: z.string().optional(),
  expectedEnvironment: z.enum(["production", "staging", "unknown"]).default("unknown"),
  exerciseWaitpoint: z.boolean().default(false),
  waitpointTimeout: z.number().int().positive().max(900).default(120),
  largeFileKiB: z.number().int().min(1).max(4096).default(256),
}).strict()

type Payload = z.infer<typeof payload>

interface Check {
  readonly name: string
  readonly ok: boolean
  readonly detail: unknown
}

export const runtimeSmoke = task({
  id: "runtime-smoke",
  sandbox: sbx,
  maxDuration: 1200,
  payload,
  run: async (input: Payload, ctx) => {
    const marker = input.marker?.trim() || `runtime-smoke-${ctx.run.id}`
    const checks: Check[] = []

    checks.push({
      name: "run-context",
      ok: true,
      detail: {
        runId: ctx.run.id,
        taskId: ctx.task.id,
        attemptId: ctx.run.attemptId ?? null,
        attemptNumber: ctx.run.attemptNumber ?? null,
        sessionId: ctx.run.sessionId ?? null,
        snapshotVersion: ctx.run.snapshotVersion ?? null,
        workspace: ctx.workspace,
      },
    })

    checks.push(await collectCheck("workspace-filesystem", () => checkWorkspace(marker, input.largeFileKiB)))
    checks.push(await collectCheck("source-bundle", () => checkBundledGuides()))
    checks.push(await collectCheck("node-version", () => checkCommand("node-version", ["node", "--version"])))
    checks.push(await collectCheck("bun-version", () => checkCommand("bun-version", ["bun", "--version"])))
    checks.push(await collectCheck("ripgrep-json", () => checkCommand("ripgrep-json", ["rg", "--json", "Helmr", "/opt/helmr-dev-workflows/guides"])))

    ctx.emit({
      type: "helmr.dev.runtime_smoke.progress",
      content: [{ type: "text", text: `completed ${checks.length} smoke checks for ${marker}` }],
    })
    ctx.log.info({
      phase: "runtime-smoke",
      scenario: input.scenario,
      expectedEnvironment: input.expectedEnvironment,
      marker,
      checks: checks.map((check) => check.name),
    })

    let waitpoint: unknown = null
    if (input.exerciseWaitpoint) {
      checks.push(await collectCheck("human-waitpoint", async () => {
        waitpoint = await ctx.wait.human<{ approved: boolean, note?: string }>({
          displayText: `Approve Helmr product smoke marker ${marker}`,
          timeout: input.waitpointTimeout,
        })
        return {
          name: "human-waitpoint",
          ok: true,
          detail: waitpoint,
        }
      }))
    }

    const failures = checks.filter((check) => !check.ok)
    const report = {
      ok: failures.length === 0,
      scenario: input.scenario,
      marker,
      expectedEnvironment: input.expectedEnvironment,
      waitpoint,
      checks,
    }
    await writeFile("runtime-smoke-report.json", `${JSON.stringify(report, null, 2)}\n`)
    if (failures.length > 0) {
      ctx.log.error({ phase: "runtime-smoke", marker, failures })
      throw new Error(`runtime smoke failed ${failures.length} check(s): ${failures.map((check) => check.name).join(", ")}`)
    }
    return report
  },
})

async function collectCheck(name: string, run: () => Promise<Check>): Promise<Check> {
  try {
    return await run()
  } catch (error) {
    return {
      name,
      ok: false,
      detail: error instanceof Error ? { message: error.message, name: error.name } : { message: String(error) },
    }
  }
}

async function checkWorkspace(marker: string, largeFileKiB: number): Promise<Check> {
  const nestedDir = "workspace-smoke/nested"
  await mkdir(nestedDir, { recursive: true })
  const id = randomUUID()
  const smallPath = `${nestedDir}/marker.txt`
  await writeFile(smallPath, `marker=${marker}\nid=${id}\n`)
  const readBack = await readFile(smallPath, "utf8")
  if (!readBack.includes(marker) || !readBack.includes(id)) {
    throw new Error("workspace marker file did not round-trip")
  }

  const largePayload = "x".repeat(largeFileKiB * 1024)
  const digest = createHash("sha256").update(largePayload).digest("hex")
  const largePath = `${nestedDir}/large-${largeFileKiB}k.txt`
  await writeFile(largePath, largePayload)
  const largeStat = await stat(largePath)
  const largeReadBack = await readFile(largePath, "utf8")
  const readDigest = createHash("sha256").update(largeReadBack).digest("hex")
  if (readDigest !== digest) {
    throw new Error("workspace large file digest mismatch")
  }

  return {
    name: "workspace-filesystem",
    ok: true,
    detail: {
      cwd: process.cwd(),
      smallPath,
      largePath,
      largeBytes: largeStat.size,
      digest,
    },
  }
}

async function checkBundledGuides(): Promise<Check> {
  const index = await readFile("/opt/helmr-dev-workflows/guides/INDEX.md", "utf8")
  const nix = await readFile("/opt/helmr-dev-workflows/guides/nix-validation.md", "utf8")
  return {
    name: "source-bundle",
    ok: true,
    detail: {
      hasGuideIndex: index.includes("Helmr"),
      hasNixGuide: nix.includes("Nix"),
    },
  }
}

function checkCommand(name: string, command: readonly string[]): Promise<Check> {
  return checkProcessCommand(command).then((result) => ({
    name,
    ok: true,
    detail: {
      command: result.command,
      output: result.output,
    },
  }))
}
