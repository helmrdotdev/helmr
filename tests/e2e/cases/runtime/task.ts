import { fixtureValue } from "../../support/runtime-mcp"
import { agent, computer, image, source, type Json } from "@helmr/sdk"
import { createHash, randomUUID } from "node:crypto"
import { mkdir, readFile, stat, writeFile } from "node:fs/promises"
import { checkCommand as checkProcessCommand } from "./process"
import { z } from "zod"

const guideInputs = source.directory("cases/runtime/guides")

const base = image("helmr-runtime-smoke")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .copy(guideInputs, "/opt/verification/inputs")
  .run([
    "sh",
    "-ceu",
    [
      "apt-get update",
      "apt-get install -y --no-install-recommends ca-certificates git jq ripgrep",
      "rm -rf /var/lib/apt/lists/*",
    ].join(" && "),
  ])
  .run(["npm", "install", "-g", "bun@1.3.13"])
  .workdir("/workspace")

export const runtimeSmokeComputer = computer({
  id: "helmr-runtime-smoke", image: base, resources: { cpu: 2, memory: "2GiB" },
})

export const runtimeSmokePayload = z.object({
  scenario: z.string().default("release-smoke"),
  marker: z.string().optional(),
  expectedComputerMarker: z.string().optional(),
  expectedEnvironment: z.enum(["production", "staging", "unknown"]).default("unknown"),
  exerciseQuestion: z.boolean().default(false),
  largeFileKiB: z.number().int().min(1).max(4096).default(256),
}).strict()

type Check = {
  readonly name: string
  readonly ok: boolean
  readonly detail: Json
}

export const runtimeSmoke = agent({
  id: "runtime-smoke",
  computer: runtimeSmokeComputer,
  maxTurnDuration: "20m",
  turn: async (turn, ctx): Promise<Json> => {
    const input = runtimeSmokePayload.parse(fixtureValue(turn.input))
    const marker = input.marker?.trim() || `runtime-smoke-${turn.id}`
    const checks: Check[] = []

    checks.push({
      name: "turn-context",
      ok: true,
      detail: {
        turnId: turn.id,
        sessionId: ctx.session.id,
        deploymentId: ctx.deployment.id,
        computer: { id: ctx.computer.id },
      },
    })

    checks.push(await collectCheck("sandbox-filesystem", () => checkComputer(marker, input.largeFileKiB, input.expectedComputerMarker)))
    checks.push(await collectCheck("source-bundle", () => checkBundledGuides()))
    checks.push(await collectCheck("node-version", () => checkCommand("node-version", ["node", "--version"])))
    checks.push(await collectCheck("bun-version", () => checkCommand("bun-version", ["bun", "--version"])))
    checks.push(await collectCheck("ripgrep-json", () => checkCommand("ripgrep-json", ["rg", "--json", "Helmr", "/opt/verification/inputs"])))

    await turn.output.write([{ type: "json", value: {
      phase: "runtime-smoke", scenario: input.scenario, marker,
      checks: checks.map(check => check.name),
    } }])
    // Process diagnostics remain internal; only selected application data above
    // is appended to the public Session timeline.
    console.info({ phase: "runtime-smoke", marker, checkCount: checks.length })
    let answer: Json = null
    if (input.exerciseQuestion) {
      checks.push(await collectCheck("human-question", async () => {
        const response = await turn.ask({
          prompt: [{ type: "text", text: `Approve product smoke marker ${marker}` }],
          answer: { type: "choice", options: [
            { id: "approve", label: "Approve", value: true },
            { id: "decline", label: "Decline", value: false },
          ], allowText: true },
        })
        answer = response.answer
        return { name: "human-question", ok: true, detail: answer }
      }))
    }

    const failures = checks.filter((check) => !check.ok)
    const report = {
      ok: failures.length === 0,
      scenario: input.scenario,
      marker,
      expectedEnvironment: input.expectedEnvironment,
      answer,
      checks,
    }
    await writeFile("runtime-smoke-report.json", `${JSON.stringify(report, null, 2)}\n`)
    if (failures.length > 0) {
      console.error(JSON.stringify({ phase: "runtime-smoke", marker, failures }))
      throw new Error(`runtime smoke failed ${failures.length} check(s): ${failures.map((check) => check.name).join(", ")}`)
    }
    await turn.output.write("Runtime checks complete")
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
      detail: error instanceof Error ? {
        message: error.message,
        name: error.name,
        ...Object.fromEntries(["code", "signal", "killed", "stdout", "stderr"].flatMap((key) => {
          const value = (error as unknown as Record<string, unknown>)[key]
          return typeof value === "string" || typeof value === "number" || typeof value === "boolean" ? [[key, value]] : []
        })),
      } : { message: String(error) },
    }
  }
}

async function checkComputer(marker: string, largeFileKiB: number, expectedPreviousMarker?: string): Promise<Check> {
  const nestedDir = "sandbox-smoke/nested"
  await mkdir(nestedDir, { recursive: true })
  const id = randomUUID()
  const smallPath = `${nestedDir}/marker.txt`
  const largePayload = "x".repeat(largeFileKiB * 1024)
  const digest = createHash("sha256").update(largePayload).digest("hex")
  const largePath = `${nestedDir}/large-${largeFileKiB}k.txt`
  let previousLargeDigest: string | null = null
  let previousLargeBytes: number | null = null
  let previousMarkerMatched = false
  if (expectedPreviousMarker !== undefined) {
    const previous = await readFile(smallPath, "utf8")
    if (!previous.includes(expectedPreviousMarker)) {
      throw new Error(`sandbox marker file did not contain expected previous marker ${expectedPreviousMarker}`)
    }
    const previousLarge = await readFile(largePath)
    previousLargeBytes = previousLarge.byteLength
    previousLargeDigest = createHash("sha256").update(previousLarge).digest("hex")
    if (previousLargeBytes !== largePayload.length || previousLargeDigest !== digest) {
      throw new Error(`sandbox prior large file mismatch: bytes=${previousLargeBytes}, digest=${previousLargeDigest}`)
    }
    previousMarkerMatched = true
  }
  await writeFile(smallPath, `marker=${marker}\nid=${id}\n`)
  const readBack = await readFile(smallPath, "utf8")
  if (!readBack.includes(marker) || !readBack.includes(id)) {
    throw new Error("sandbox marker file did not round-trip")
  }

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
      previousLargeDigest,
      previousLargeBytes,
    },
  }
}

async function checkBundledGuides(): Promise<Check> {
  const index = await readFile("/opt/verification/inputs/INDEX.md", "utf8")
  const nix = await readFile("/opt/verification/inputs/nix-validation.md", "utf8")
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
