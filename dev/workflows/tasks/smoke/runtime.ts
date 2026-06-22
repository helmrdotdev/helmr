import { cache, channel, image, logger, metadata, sandbox, source, task, wait } from "@helmr/sdk"
import { createHash, randomUUID } from "node:crypto"
import { mkdir, readFile, stat, writeFile } from "node:fs/promises"
import { checkCommand as checkProcessCommand } from "../lib/process"
import { z } from "zod"

const dependencyInputs = source.directory(".", {
  ignore: ["*", "!package.json", "!bun.lock", "!tsconfig.json", "!vendor", "!vendor/**"],
})
const guideInputs = source.directory("guides")

const base = image("helmr-runtime-smoke")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .copy("/opt/helmr-task", dependencyInputs)
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
  .workdir("/opt/helmr-task")
  .run(["bun", "install", "--frozen-lockfile"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("runtime-smoke-bun") }],
  })
  .workdir("/workspace")

const sbx = sandbox("helmr-runtime-smoke")
  .image(base)
  .resources({ cpu: 2, memory: "2Gi", disk: "16Gi" })

const payload = z.object({
  scenario: z.string().default("release-smoke"),
  marker: z.string().optional(),
  expectedWorkspaceMarker: z.string().optional(),
  expectedEnvironment: z.enum(["production", "staging", "unknown"]).default("unknown"),
  exerciseWaitpoint: z.boolean().default(false),
  waitpointTokenId: z.string().optional(),
  waitpointTimeout: z.number().int().positive().max(900).default(120),
  largeFileKiB: z.number().int().min(1).max(4096).default(256),
}).strict()

type Payload = z.infer<typeof payload>

const approvalDecision = z.object({
  approved: z.boolean(),
  note: z.string().optional(),
})

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
        sessionId: ctx.session.id,
        snapshotVersion: ctx.run.snapshotVersion ?? null,
        workspace: ctx.workspace,
      },
    })

    checks.push(await collectCheck("workspace-filesystem", () => checkWorkspace(marker, input.largeFileKiB, input.expectedWorkspaceMarker)))
    checks.push(await collectCheck("source-bundle", () => checkBundledGuides()))
    checks.push(await collectCheck("node-version", () => checkCommand("node-version", ["node", "--version"])))
    checks.push(await collectCheck("bun-version", () => checkCommand("bun-version", ["bun", "--version"])))
    checks.push(await collectCheck("ripgrep-json", () => checkCommand("ripgrep-json", ["rg", "--json", "Helmr", "/opt/helmr-dev-workflows/guides"])))

    await metadata.set("runtimeSmoke", {
      marker,
      completedChecks: checks.length,
      expectedEnvironment: input.expectedEnvironment,
    })
    logger.info({
      phase: "runtime-smoke",
      scenario: input.scenario,
      expectedEnvironment: input.expectedEnvironment,
      marker,
      checks: checks.map((check) => check.name),
    })
    await ctx.session.output(channel.output("runtime-smoke.progress", { schema: z.unknown() })).append({
      marker,
      scenario: input.scenario,
      completedChecks: checks.length,
    })

    let waitpoint: unknown = null
    if (input.exerciseWaitpoint) {
      checks.push(await collectCheck("human-waitpoint", async () => {
        const token = input.waitpointTokenId === undefined
          ? await wait.createToken({
              timeout: input.waitpointTimeout,
              tags: ["smoke", "runtime"],
              metadata: { marker, subject: `Approve Helmr product smoke marker ${marker}` },
            })
          : { id: input.waitpointTokenId }
        waitpoint = await wait.forToken(token, {
          schema: approvalDecision,
          timeout: input.waitpointTimeout,
          tags: ["smoke", "runtime"],
          metadata: { marker, subject: `Approve Helmr product smoke marker ${marker}` },
        }).unwrap()
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
    await ctx.session.output(channel.output("runtime-smoke.report", { schema: z.unknown() })).append(report, { contentType: "application/vnd.helmr.runtime-smoke+json" })
    if (failures.length > 0) {
      logger.error({ phase: "runtime-smoke", marker, failures })
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

async function checkWorkspace(marker: string, largeFileKiB: number, expectedPreviousMarker?: string): Promise<Check> {
  const nestedDir = "workspace-smoke/nested"
  await mkdir(nestedDir, { recursive: true })
  const id = randomUUID()
  const smallPath = `${nestedDir}/marker.txt`
  let previousMarkerMatched = false
  if (expectedPreviousMarker !== undefined) {
    const previous = await readFile(smallPath, "utf8")
    if (!previous.includes(expectedPreviousMarker)) {
      throw new Error(`workspace marker file did not contain expected previous marker ${expectedPreviousMarker}`)
    }
    previousMarkerMatched = true
  }
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
      previousMarkerMatched,
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
