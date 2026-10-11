export function baseAgentEnv(): Record<string, string> {
  const env: Record<string, string> = {}
  for (const key of ["HOME", "PATH", "TMPDIR", "USER", "LOGNAME", "LANG", "LC_ALL"]) {
    const value = process.env[key]
    if (typeof value === "string") {
      env[key] = value
    }
  }
  return env
}
