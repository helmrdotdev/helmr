export interface ClientSmokeConfig {
  readonly apiUrl: string
  readonly apiKey: string
  readonly credentialKind: "api_key" | "session"
  readonly projectId: string
  readonly environmentId: string
  readonly marker: string
  readonly workspaceId?: string
  readonly taskId: "runtime-smoke"
  readonly sandboxId: "helmr-runtime-smoke"
}

export function readConfig(): ClientSmokeConfig {
  const apiUrl = requiredEnv("HELMR_API_URL")
  const apiKey = requiredEnv("HELMR_API_KEY")
  const credentialKind = apiKey.startsWith("hlmr_sk_") ? "api_key" : "session"
  const projectId = process.env["HELMR_PROJECT"]?.trim() || "helmr"
  const environmentId = process.env["HELMR_ENV"]?.trim() || "production"
  const marker = process.env["SMOKE_MARKER"]?.trim() || `workspace-lifecycle-${timestamp()}`
  const workspaceId = process.env["HELMR_WORKSPACE_ID"]?.trim()
  return {
    apiUrl,
    apiKey,
    credentialKind,
    projectId,
    environmentId,
    marker,
    ...(workspaceId === undefined || workspaceId === "" ? {} : { workspaceId }),
    taskId: "runtime-smoke",
    sandboxId: "helmr-runtime-smoke",
  }
}

export function requestScope(config: ClientSmokeConfig): { readonly projectId?: string; readonly environmentId?: string } {
  if (config.credentialKind === "api_key") {
    return {}
  }
  return { projectId: config.projectId, environmentId: config.environmentId }
}

export function currentDeploymentURL(config: ClientSmokeConfig): string {
  const baseURL = config.apiUrl.replace(/\/+$/, "")
  if (config.credentialKind === "api_key") {
    return `${baseURL}/api/deployments/current`
  }
  return `${baseURL}/api/projects/${encodeURIComponent(config.projectId)}/environments/${encodeURIComponent(config.environmentId)}/deployments/current`
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
