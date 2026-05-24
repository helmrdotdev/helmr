type EnvLike = Record<string, string | undefined>

export function compactEnv(env: EnvLike): Record<string, string> {
  return Object.fromEntries(
    Object.entries(env).filter((entry): entry is [string, string] => typeof entry[1] === "string"),
  )
}
