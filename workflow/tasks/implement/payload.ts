import type { AuthSecrets } from "./types"

export function readAuthSecrets(): AuthSecrets {
  requireEnv("ANTHROPIC_API_KEY")
  return {
    openaiApiKey: requireEnv("OPENAI_API_KEY"),
    cursorApiKey: requireEnv("CURSOR_API_KEY"),
    githubToken: requireEnv("GITHUB_TOKEN"),
  }
}

function requireEnv(name: string): string {
  const value = process.env[name]
  if (!value) {
    throw new Error(`${name} secret is required`)
  }
  return value
}
