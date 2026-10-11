/** A positive integer unit string, or a positive integer number of milliseconds. */
export type Duration = string | number

export function durationMilliseconds(value: Duration, label = "Duration"): number {
  let milliseconds: bigint
  if (typeof value === "number") {
    if (!Number.isSafeInteger(value) || value <= 0) throw new Error(`${label} must be positive safe integer milliseconds`)
    return value
  }
  if (typeof value !== "string") throw new Error(`${label} must be a positive duration`)
  const match = /^([1-9][0-9]*)(ms|s|m|h|d)$/.exec(value)
  if (!match) throw new Error(`${label} must be a positive integer duration (ms, s, m, h or d)`)
  const units: Record<string, bigint> = { ms: 1n, s: 1000n, m: 60000n, h: 3600000n, d: 86400000n }
  milliseconds = BigInt(match[1]!) * units[match[2]!]!
  if (milliseconds > BigInt(Number.MAX_SAFE_INTEGER)) throw new Error(`${label} must be positive safe integer milliseconds`)
  return Number(milliseconds)
}
