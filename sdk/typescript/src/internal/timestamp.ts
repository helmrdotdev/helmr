const utcRFC3339 = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d{1,9})?Z$/

export function timestampString(value: unknown, label: string): string {
  const match = typeof value === "string" ? utcRFC3339.exec(value) : null
  if (match === null || !validDateTime(match)) {
    throw new Error(`${label} must be a UTC RFC 3339 timestamp`)
  }
  return value as string
}

function validDateTime(match: RegExpExecArray): boolean {
  const year = Number(match[1])
  const month = Number(match[2])
  const day = Number(match[3])
  const hour = Number(match[4])
  const minute = Number(match[5])
  const second = Number(match[6])
  if (
    month < 1 || month > 12 ||
    hour > 23 || minute > 59 || second > 59
  ) {
    return false
  }
  const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0)
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31]
  return day >= 1 && day <= days[month - 1]!
}
