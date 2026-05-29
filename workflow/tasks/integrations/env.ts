type EnvLike = Record<string, string | undefined>

export function compactEnv(env: EnvLike): Record<string, string> {
  return Object.fromEntries(
    Object.entries(env).filter((entry): entry is [string, string] => typeof entry[1] === "string"),
  )
}

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

export async function withProcessEnv<T>(env: Record<string, string>, callback: () => Promise<T>): Promise<T> {
  const original = { ...process.env }
  for (const key of Object.keys(process.env)) {
    delete process.env[key]
  }
  Object.assign(process.env, env)
  try {
    return await callback()
  } finally {
    for (const key of Object.keys(process.env)) {
      delete process.env[key]
    }
    Object.assign(process.env, original)
  }
}

export function requiredProcessEnv(key: string): string {
  const value = process.env[key]
  if (!value) {
    throw new Error(`${key} is required`)
  }
  return value
}
