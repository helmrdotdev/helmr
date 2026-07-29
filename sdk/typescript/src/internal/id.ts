const uuidV7Pattern =
  /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/

export function resourceID(value: unknown, label: string): string {
  if (typeof value !== "string" || !uuidV7Pattern.test(value)) {
    throw new Error(`${label} must be a canonical UUIDv7`)
  }
  return value
}
