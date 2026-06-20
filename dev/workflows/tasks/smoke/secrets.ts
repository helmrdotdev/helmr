import { cache, image, logger, sandbox, source, task } from "@helmr/sdk"
import { z } from "zod"

const dependencyInputs = source.directory(".", {
  ignore: ["*", "!package.json", "!bun.lock", "!tsconfig.json", "!vendor", "!vendor/**"],
})

const base = image("helmr-secret-smoke")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .copy("/opt/helmr-task", dependencyInputs)
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .workdir("/opt/helmr-task")
  .run(["bun", "install", "--frozen-lockfile"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("secret-smoke-bun") }],
  })
  .workdir("/workspace")

const sbx = sandbox("helmr-secret-smoke")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi", disk: "8Gi" })

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
  sandbox: sbx,
  maxDuration: 300,
  secrets: [
    { name: "ANTHROPIC_API_KEY", env: "ANTHROPIC_API_KEY" },
    { name: "CURSOR_API_KEY", env: "CURSOR_API_KEY" },
    { name: "GITHUB_TOKEN", env: "GITHUB_TOKEN" },
    { name: "OPENAI_API_KEY", env: "OPENAI_API_KEY" },
  ],
  payload,
  run: async (input, ctx) => {
    const secrets = Object.fromEntries(secretNames.map((name) => [name, secretFingerprint(name)]))
    logger.info({
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

function secretFingerprint(name: typeof secretNames[number]): { present: true } {
  const value = process.env[name]
  if (!value) {
    throw new Error(`${name} was not injected`)
  }
  return { present: true }
}
