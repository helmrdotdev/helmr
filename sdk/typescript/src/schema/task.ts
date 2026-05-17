export const TASK_ID_PATTERN = "^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$" as const
export const TASK_ID_MAX_LENGTH = 128
export const DEFAULT_MAX_DURATION_SECONDS = 900
export const MIN_MAX_DURATION_SECONDS = 5
export const MAX_DURATION_SECONDS = 86400

export class TaskIdError extends Error {
  override readonly name = "TaskIdError"
  readonly value: string

  constructor(value: string) {
    super(`task id must match ${TASK_ID_PATTERN}: ${JSON.stringify(value)}`)
    this.value = value
  }
}

export function validateTaskId(value: string): void {
  if (!isValidTaskId(value)) {
    throw new TaskIdError(value)
  }
}

export function isValidTaskId(value: string): boolean {
  if (value.length === 0 || value.length > TASK_ID_MAX_LENGTH) {
    return false
  }
  const first = value.charCodeAt(0)
  if (!isAsciiAlnum(first)) {
    return false
  }
  for (let index = 1; index < value.length; index += 1) {
    const code = value.charCodeAt(index)
    if (!(isAsciiAlnum(code) || code === 0x2e || code === 0x5f || code === 0x2d)) {
      return false
    }
  }
  return true
}

export class TaskMaxDurationError extends Error {
  override readonly name = "TaskMaxDurationError"
  readonly value: unknown
  readonly label: string

  constructor(value: unknown, label: string = "task maxDuration") {
    super(
      `${label} must be an integer number of seconds between ${MIN_MAX_DURATION_SECONDS} and ${MAX_DURATION_SECONDS}`,
    )
    this.value = value
    this.label = label
  }
}

export function readOptionalMaxDurationSeconds(
  value: unknown,
  label = "task maxDuration",
): number {
  if (value === undefined) {
    return DEFAULT_MAX_DURATION_SECONDS
  }
  if (
    typeof value === "number" &&
    Number.isInteger(value) &&
    Number.isFinite(value) &&
    value >= MIN_MAX_DURATION_SECONDS &&
    value <= MAX_DURATION_SECONDS
  ) {
    return value
  }
  throw new TaskMaxDurationError(value, label)
}

export function validateOptionalMaxDurationSeconds(value: unknown, label = "task maxDuration"): void {
  readOptionalMaxDurationSeconds(value, label)
}

function isAsciiAlnum(code: number): boolean {
  return (code >= 0x30 && code <= 0x39) || (code >= 0x41 && code <= 0x5a) || (code >= 0x61 && code <= 0x7a)
}
