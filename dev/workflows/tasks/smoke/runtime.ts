import {
  image,
  logger,
  metadata,
  source,
  task,
  tokens,
  sandbox,
  type JsonValue,
} from "@helmr/sdk"
import { createHash, randomUUID } from "node:crypto"
import { mkdir, readFile, stat, writeFile } from "node:fs/promises"
import { checkCommand as checkProcessCommand } from "../lib/process"
import { z } from "zod"

const guideInputs = source.directory("guides")

const base = image("helmr-runtime-smoke")
  .from("node:24-bookworm-slim")
  .workdir("/sandbox")
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
  .workdir("/sandbox")

export const runtimeSmokeWorkspace = sandbox({ id: "helmr-runtime-smoke" })
  .image(base)
  .resources({ cpu: 2, memory: "2GiB" })

const payload = z.object({
  scenario: z.string().default("release-smoke"),
  marker: z.string().optional(),
  expectedWorkspaceMarker: z.string().optional(),
  expectedEnvironment: z.enum(["production", "staging", "unknown"]).default("unknown"),
  exerciseToken: z.boolean().default(false),
  externalTokenId: z.string().regex(/^tok_[a-z2-7]{26}$/).optional(),
  tokenTimeout: z.number().int().positive().max(900).default(120),
  largeFileKiB: z.number().int().min(1).max(4096).default(256),
}).strict()

type Payload = z.infer<typeof payload>

const approvalDecision = z.object({
  approved: z.boolean(),
  note: z.string().optional(),
})

type Check = {
  readonly name: string
  readonly ok: boolean
  readonly detail: JsonValue
}

export const runtimeSmoke = task({
  id: "runtime-smoke",
  maxDuration: "20m",
  payload,
  run: async (input: Payload, ctx): Promise<JsonValue> => {
    const marker = input.marker?.trim() || `runtime-smoke-${ctx.run.id}`
    const checks: Check[] = []

    checks.push({
      name: "run-context",
      ok: true,
      detail: {
        runId: ctx.run.id,
        attemptNumber: ctx.run.attemptNumber,
        deploymentId: ctx.deployment.id,
        deploymentVersion: ctx.deployment.version,
        workspace: ctx.workspace === null
          ? null
          : {
              id: ctx.workspace.id,
              attemptBaseVersionId: ctx.workspace.attemptBaseVersionId,
            },
      },
    })

    checks.push(await collectCheck("sandbox-filesystem", () => checkWorkspace(marker, input.largeFileKiB, input.expectedWorkspaceMarker)))
    checks.push(await collectCheck("source-bundle", () => checkBundledGuides()))
    checks.push(await collectCheck("node-version", () => checkCommand("node-version", ["node", "--version"])))
    checks.push(await collectCheck("bun-version", () => checkCommand("bun-version", ["bun", "--version"])))
    checks.push(await collectCheck("ripgrep-json", () => checkCommand("ripgrep-json", ["rg", "--json", "Helmr", "/opt/helmr-dev-workflows/guides"])))

    await metadata.set("smoke.phase", "running")
    await metadata.patch({
      "smoke.marker": marker,
      "smoke.scenario": input.scenario,
    })
    await metadata.increment("smoke.checks", checks.length)
    await logger.debug("runtime smoke diagnostics complete", {
      marker,
      checkCount: checks.length,
    })
    await logger.info("runtime smoke reached token phase", {
      marker,
      exerciseToken: input.exerciseToken,
    })
    await logger.warn("runtime smoke warning probe", {
      marker,
      expected: true,
    })
    await logger.error("runtime smoke error-level probe", {
      marker,
      expected: true,
    })

    console.info({
      phase: "runtime-smoke",
      scenario: input.scenario,
      expectedEnvironment: input.expectedEnvironment,
      marker,
      checks: checks.map((check) => check.name),
    })
    let tokenResult: JsonValue = null
    if (input.exerciseToken) {
      checks.push(await collectCheck("human-token", async () => {
        const timeout = `${input.tokenTimeout}s`
        const token = input.externalTokenId === undefined
          ? await tokens.create({
              timeout,
              tags: ["smoke", "runtime"],
              metadata: { marker, subject: `Approve Helmr product smoke marker ${marker}` },
            })
          : tokens.ref(input.externalTokenId)
        tokenResult = await token.wait({
          schema: approvalDecision,
          timeout,
          tags: ["smoke", "runtime"],
          metadata: { marker, subject: `Approve Helmr product smoke marker ${marker}` },
        }).unwrap()
        return {
          name: "human-token",
          ok: true,
          detail: tokenResult,
        }
      }))
    }

    const failures = checks.filter((check) => !check.ok)
    const report = {
      ok: failures.length === 0,
      scenario: input.scenario,
      marker,
      expectedEnvironment: input.expectedEnvironment,
      token: tokenResult,
      checks,
    }
    await writeFile("runtime-smoke-report.json", `${JSON.stringify(report, null, 2)}\n`)
    if (failures.length > 0) {
      console.error({ phase: "runtime-smoke", marker, failures })
      throw new Error(`runtime smoke failed ${failures.length} check(s): ${failures.map((check) => check.name).join(", ")}`)
    }
    await metadata.set("smoke.phase", "complete")
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
  const nestedDir = "sandbox-smoke/nested"
  await mkdir(nestedDir, { recursive: true })
  const id = randomUUID()
  const smallPath = `${nestedDir}/marker.txt`
  let previousMarkerMatched = false
  if (expectedPreviousMarker !== undefined) {
    const previous = await readFile(smallPath, "utf8")
    if (!previous.includes(expectedPreviousMarker)) {
      throw new Error(`sandbox marker file did not contain expected previous marker ${expectedPreviousMarker}`)
    }
    previousMarkerMatched = true
  }
  await writeFile(smallPath, `marker=${marker}\nid=${id}\n`)
  const readBack = await readFile(smallPath, "utf8")
  if (!readBack.includes(marker) || !readBack.includes(id)) {
    throw new Error("sandbox marker file did not round-trip")
  }

  const largePayload = "x".repeat(largeFileKiB * 1024)
  const digest = createHash("sha256").update(largePayload).digest("hex")
  const largePath = `${nestedDir}/large-${largeFileKiB}k.txt`
  await writeFile(largePath, largePayload)
  const largeStat = await stat(largePath)
  const largeReadBack = await readFile(largePath, "utf8")
  const readDigest = createHash("sha256").update(largeReadBack).digest("hex")
  if (readDigest !== digest) {
    throw new Error("sandbox large file digest mismatch")
  }

  return {
    name: "sandbox-filesystem",
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
