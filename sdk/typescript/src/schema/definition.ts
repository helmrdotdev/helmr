const DEFINITION_ID_PATTERN = "^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$" as const
const DEFINITION_ID_MAX_LENGTH = 128
class DefinitionIdError extends Error {
  override readonly name = "DefinitionIdError"
  readonly value: string

  constructor(value: string) {
    super(`definition id must match ${DEFINITION_ID_PATTERN}: ${JSON.stringify(value)}`)
    this.value = value
  }
}

export function validateDefinitionId(value: string): void {
  if (!isValidDefinitionId(value)) {
    throw new DefinitionIdError(value)
  }
}

function isValidDefinitionId(value: string): boolean {
  if (value.length === 0 || value.length > DEFINITION_ID_MAX_LENGTH) {
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

function isAsciiAlnum(code: number): boolean {
  return (code >= 0x30 && code <= 0x39) || (code >= 0x41 && code <= 0x5a) || (code >= 0x61 && code <= 0x7a)
}
