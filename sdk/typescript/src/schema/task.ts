const TASK_ID_PATTERN = "^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$" as const
const TASK_ID_MAX_LENGTH = 128
const QUEUE_NAME_PATTERN = "^[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$" as const
const QUEUE_NAME_MAX_LENGTH = 256

class TaskIdError extends Error {
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

function isValidTaskId(value: string): boolean {
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

class TaskQueueNameError extends Error {
  override readonly name = "TaskQueueNameError"
  readonly value: string

  constructor(value: string) {
    super(`queue name must match ${QUEUE_NAME_PATTERN}: ${JSON.stringify(value)}`)
    this.value = value
  }
}

class TaskQueueConcurrencyLimitError extends Error {
  override readonly name = "TaskQueueConcurrencyLimitError"
  readonly value: unknown

  constructor(value: unknown) {
    super("queue concurrencyLimit must be a positive integer")
    this.value = value
  }
}

export function validateQueueName(value: string): void {
  if (!isValidQueueName(value)) {
    throw new TaskQueueNameError(value)
  }
}

function isValidQueueName(value: string): boolean {
  if (value.length === 0 || value.length > QUEUE_NAME_MAX_LENGTH) {
    return false
  }
  const first = value.charCodeAt(0)
  if (!isAsciiAlnum(first)) {
    return false
  }
  for (let index = 1; index < value.length; index += 1) {
    const code = value.charCodeAt(index)
    if (!(isAsciiAlnum(code) || code === 0x2e || code === 0x5f || code === 0x2d || code === 0x2f)) {
      return false
    }
  }
  return true
}

export function validateOptionalQueueConcurrencyLimit(value: unknown): void {
  if (value === undefined || value === null) {
    return
  }
  if (typeof value === "number" && Number.isSafeInteger(value) && value > 0) {
    return
  }
  throw new TaskQueueConcurrencyLimitError(value)
}

function isAsciiAlnum(code: number): boolean {
  return (code >= 0x30 && code <= 0x39) || (code >= 0x41 && code <= 0x5a) || (code >= 0x61 && code <= 0x7a)
}
