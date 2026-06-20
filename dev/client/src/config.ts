export interface ClientSmokeConfig {
  readonly apiUrl: string
  readonly apiKey: string
  readonly marker: string
  readonly taskId: "runtime-smoke"
  readonly sandboxId: "helmr-runtime-smoke"
}

export function readConfig(): ClientSmokeConfig {
  const apiUrl = requiredEnv("HELMR_API_URL")
  const apiKey = requiredEnv("HELMR_API_KEY")
  const marker = process.env["SMOKE_MARKER"]?.trim() || `workspace-lifecycle-${timestamp()}`
  return {
    apiUrl,
    apiKey,
    marker,
    taskId: "runtime-smoke",
    sandboxId: "helmr-runtime-smoke",
  }
}

function requiredEnv(name: string): string {
  const value = process.env[name]?.trim()
  if (value === undefined || value === "") {
    throw new Error(`${name} is required`)
  }
  return value
}

function timestamp(): string {
  return new Date().toISOString().replaceAll(/\D/g, "").slice(0, 14)
}
