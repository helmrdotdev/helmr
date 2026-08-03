export interface ClientSmokeConfig {
  readonly apiUrl: string
  readonly apiKey: string
  readonly marker: string
  readonly secretName?: string
}

export function readConfig(): ClientSmokeConfig {
  const apiUrl = requiredEnv("HELMR_API_URL")
  const apiKey = requiredEnv("HELMR_API_KEY")
  const marker = process.env["SMOKE_MARKER"]?.trim() || `workspace-basic-exec-${timestamp()}`
  const secretName = process.env["HELMR_SMOKE_SECRET_NAME"]?.trim()
  return {
    apiUrl,
    apiKey,
    marker,
    ...(secretName === undefined || secretName === "" ? {} : { secretName }),
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
