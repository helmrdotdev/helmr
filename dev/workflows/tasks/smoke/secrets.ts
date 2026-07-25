import { image, task, workspace } from "@helmr/sdk"
import { z } from "zod"

const base = image("helmr-secret-smoke")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .workdir("/workspace")

export const secretSmokeWorkspace = workspace("helmr-secret-smoke")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi" })

const payload = z.object({
  scenario: z.string().default("secret-smoke"),
  expectedEnvironment: z.enum(["production", "staging", "unknown"]).default("unknown"),
}).strict()

const secretNames = [
  "ANTHROPIC_API_KEY",
  "CURSOR_API_KEY",
  "GITHUB_TOKEN",
  "OPENAI_API_KEY",
] as const

export const secretSmoke = task({
  id: "secret-smoke",
  maxDuration: "5m",
  payload,
  run: async (input, ctx) => {
    const secrets = Object.fromEntries(secretNames.map((name) => [name, secretFingerprint(name)]))
    console.info({
      phase: "secret-smoke",
      scenario: input.scenario,
      expectedEnvironment: input.expectedEnvironment,
      injectedSecrets: secretNames,
    })
    return {
      ok: true,
      scenario: input.scenario,
      expectedEnvironment: input.expectedEnvironment,
      runId: ctx.run.id,
      secrets,
    }
  },
})

export const missingSecretSmoke = task({
  id: "missing-secret-smoke",
  maxDuration: "5m",
  payload,
  run: async () => {
    throw new Error("missing-secret-smoke should be rejected before run creation")
  },
})

function secretFingerprint(name: typeof secretNames[number]): { present: true } {
  const value = process.env[name]
  if (!value) {
    throw new Error(`${name} was not injected`)
  }
  return { present: true }
}
